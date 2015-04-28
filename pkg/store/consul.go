package store

import (
	"crypto/tls"
	"errors"
	"net/http"
	"time"

	log "github.com/Sirupsen/logrus"
	api "github.com/hashicorp/consul/api"
)

var (
	// ErrSessionUndefined is exported
	ErrSessionUndefined = errors.New("Session does not exist")
)

// Consul embeds the client and watches/lock sessions
type Consul struct {
	config   *api.Config
	client   *api.Client
	sessions map[string]*api.Session
	watches  map[string]*Watch
}

// Watch embeds the event channel and the
// refresh interval
type Watch struct {
	LastIndex uint64
	Interval  time.Duration
	EventChan interface{}
}

// InitializeConsul creates a new Consul client given
// a list of endpoints and optional tls config
func InitializeConsul(endpoints []string, options ...interface{}) (Store, error) {
	s := &Consul{}
	s.sessions = make(map[string]*api.Session)
	s.watches = make(map[string]*Watch)

	// Create Consul client
	config := api.DefaultConfig()
	s.config = config
	config.HttpClient = http.DefaultClient
	config.Address = endpoints[0]
	config.Scheme = "http"

	// Sets all the options
	s.SetOptions(options...)

	// Creates a new client
	client, err := api.NewClient(config)
	if err != nil {
		log.Errorf("Couldn't initialize consul client..")
		return nil, err
	}
	s.client = client

	return s, nil
}

// SetOptions sets the options for Consul
func (s *Consul) SetOptions(options ...interface{}) {
	for _, opt := range options {

		switch opt := opt.(type) {

		case *tls.Config:
			s.SetTLS(opt)

		case time.Duration:
			s.SetTimeout(opt)

		default:
			// TODO give more meaningful information to print
			log.Info("store: option unsupported for consul")

		}
	}
}

// SetTLS sets Consul TLS options
func (s *Consul) SetTLS(tls *tls.Config) {
	if tls != nil {
		s.config.HttpClient.Transport = &http.Transport{
			TLSClientConfig: tls,
		}
		s.config.Scheme = "https"
	}
}

// SetTimeout sets the timout for connecting to Consul
func (s *Consul) SetTimeout(time time.Duration) {
	s.config.WaitTime = time
}

// Get the value at "key", returns the last modified index
// to use in conjunction to CAS calls
func (s *Consul) Get(key string) (value []byte, lastIndex uint64, err error) {
	pair, meta, err := s.client.KV().Get(partialFormat(key), nil)
	if err != nil {
		return nil, 0, err
	}
	if pair == nil {
		return nil, 0, ErrKeyNotFound
	}
	return pair.Value, meta.LastIndex, nil
}

// Put a value at "key"
func (s *Consul) Put(key string, value []byte) error {
	p := &api.KVPair{Key: partialFormat(key), Value: value}
	if s.client == nil {
		log.Error("Error initializing client")
	}
	_, err := s.client.KV().Put(p, nil)
	return err
}

// Delete a value at "key"
func (s *Consul) Delete(key string) error {
	_, err := s.client.KV().Delete(partialFormat(key), nil)
	return err
}

// Exists checks that the key exists inside the store
func (s *Consul) Exists(key string) (bool, error) {
	_, _, err := s.Get(key)
	if err != nil && err == ErrKeyNotFound {
		return false, err
	}
	return true, nil
}

// GetRange gets a range of values at "directory"
func (s *Consul) GetRange(prefix string) (values [][]byte, err error) {
	pairs, _, err := s.client.KV().List(partialFormat(prefix), nil)
	if err != nil {
		return nil, err
	}
	for _, pair := range pairs {
		if pair.Key == prefix {
			continue
		}
		values = append(values, pair.Value)
	}
	return values, nil
}

// DeleteRange deletes a range of values at "directory"
func (s *Consul) DeleteRange(prefix string) error {
	_, err := s.client.KV().DeleteTree(partialFormat(prefix), nil)
	return err
}

// Watch a single key for modifications
func (s *Consul) Watch(key string, heartbeat time.Duration, callback WatchCallback) error {
	key = partialFormat(key)
	interval := heartbeat
	eventChan := s.waitForChange(key)
	s.watches[key] = &Watch{Interval: interval, EventChan: eventChan}

	for _ = range eventChan {
		log.WithField("name", "consul").Debug("Key watch triggered")
		entry, _, err := s.Get(key)
		if err != nil {
			log.Error("Cannot refresh the key: ", key, ", cancelling watch")
			s.watches[key] = nil
			return err
		}

		value := [][]byte{[]byte(entry)}
		callback(value)
	}

	return nil
}

// CancelWatch cancels a watch, sends a signal to the appropriate
// stop channel
func (s *Consul) CancelWatch(key string) error {
	key = partialFormat(key)
	if _, ok := s.watches[key]; !ok {
		log.Error("Chan does not exist for key: ", key)
		return ErrWatchDoesNotExist
	}
	s.watches[key] = nil
	return nil
}

// Internal function to check if a key has changed
func (s *Consul) waitForChange(key string) <-chan uint64 {
	ch := make(chan uint64)
	go func() {
		for {
			watch, ok := s.watches[key]
			if !ok {
				log.Error("Cannot access last index for key: ", key, " closing channel")
				break
			}
			option := &api.QueryOptions{
				WaitIndex: watch.LastIndex,
				WaitTime:  watch.Interval}
			_, meta, err := s.client.KV().Get(key, option)
			if err != nil {
				log.WithField("name", "consul").Errorf("Discovery error: %v", err)
				break
			}
			watch.LastIndex = meta.LastIndex
			ch <- watch.LastIndex
		}
		close(ch)
	}()
	return ch
}

// WatchRange triggers a watch on a range of values at "directory"
func (s *Consul) WatchRange(prefix string, filter string, heartbeat time.Duration, callback WatchCallback) error {
	prefix = partialFormat(prefix)
	interval := heartbeat
	eventChan := s.waitForChange(prefix)
	s.watches[prefix] = &Watch{Interval: interval, EventChan: eventChan}

	for _ = range eventChan {
		log.WithField("name", "consul").Debug("Key watch triggered")
		values, err := s.GetRange(prefix)
		if err != nil {
			log.Error("Cannot refresh keys with prefix: ", prefix, ", cancelling watch")
			s.watches[prefix] = nil
			return err
		}
		callback(values)
	}

	return nil
}

// CancelWatchRange stops the watch on the range of values, sends
// a signal to the appropriate stop channel
func (s *Consul) CancelWatchRange(prefix string) error {
	return s.CancelWatch(prefix)
}

// Acquire the lock for "key"/"directory"
func (s *Consul) Acquire(key string, value []byte) (string, error) {
	key = partialFormat(key)
	session := s.client.Session()
	id, _, err := session.CreateNoChecks(nil, nil)
	if err != nil {
		return "", err
	}

	// Add session to map
	s.sessions[id] = session

	p := &api.KVPair{Key: key, Value: value, Session: id}
	if work, _, err := s.client.KV().Acquire(p, nil); err != nil {
		return "", err
	} else if !work {
		return "", ErrCannotLock
	}

	return id, nil
}

// Release the lock for "key"/"directory"
func (s *Consul) Release(id string) error {
	if _, ok := s.sessions[id]; !ok {
		log.Error("Lock session does not exist")
		return ErrSessionUndefined
	}
	session := s.sessions[id]
	session.Destroy(id, nil)
	s.sessions[id] = nil
	return nil
}

// AtomicPut put a value at "key" if the key has not been
// modified in the meantime, throws an error if this is the case
func (s *Consul) AtomicPut(key string, _ []byte, newValue []byte, index uint64) (bool, error) {
	p := &api.KVPair{Key: partialFormat(key), Value: newValue, ModifyIndex: index}
	if work, _, err := s.client.KV().CAS(p, nil); err != nil {
		return false, err
	} else if !work {
		return false, ErrKeyModified
	}
	return true, nil
}

// AtomicDelete deletes a value at "key" if the key has not
// been modified in the meantime, throws an error if this is the case
func (s *Consul) AtomicDelete(key string, oldValue []byte, index uint64) (bool, error) {
	p := &api.KVPair{Key: partialFormat(key), ModifyIndex: index}
	if work, _, err := s.client.KV().DeleteCAS(p, nil); err != nil {
		return false, err
	} else if !work {
		return false, ErrKeyModified
	}
	return true, nil
}
