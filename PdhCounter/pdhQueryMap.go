// +build windows
package PdhCounter

import (
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
)

type QueryError struct {
	Host string
	Err error
}

func (e QueryError) Error() string {
	return fmt.Sprintf("query error for %s -> %v", e.Host, e.Err)
}

// pdhQueryMap is a concurrency-safe map of hosts and their pdhQueries.
type pdhQueryMap struct {
	sync.RWMutex
	CancelChan				chan struct{} // a channel that when closed will signal to pdhQueryMap to stop collection
	logger					*logrus.Logger	// a logger to which all log messages will be written to
	m 						map[string]*pdhQuery
	NewQueryChan 			chan QueryResult
}

// NewClientMap will create a new pdhQueryMap
func NewPdhQueryMap(logger *logrus.Logger) *pdhQueryMap {
	p := pdhQueryMap{
		CancelChan: make(chan struct{}),
		logger:logger,
		m: make(map[string]*pdhQuery),
		NewQueryChan: make(chan QueryResult),
	}

	return &p
}

func (p *pdhQueryMap) Listen() error {
	for {
		select {
		case <-p.CancelChan:
			test := 2
			fmt.Println("shutting down pdhQueryMap", test)
			p.logger.Info("shutting down pdhQueryMap")
			return nil
		case result := <-p.NewQueryChan:
			go func(result QueryResult) {
				defer func() {
					p.Delete(result.Host)
					p.logger.WithField("host", result.Host).Info("query collection stopped")
				}()

				// Check if an equivalent query already exists for this host
				if q := p.GetQuery(result.Host); q != nil {
					if !q.TestEquivalence(result.Query) {
						// stop the old query
						q.Stop()
					} else {
						return
					}
				}

				if result.Query.NumCounters() > 0 {
					p.Add(result.Host, result.Query)

					if err := result.Query.Start(); err != nil {
						p.logger.Error(QueryError{result.Host, err})
					}
				}
			}(result)
		}
	}
}

// Length will return the number of keys within c.m
func (c *pdhQueryMap) Length() int {
	return len(c.m)
}

// GetQuery will retrieve the value corresponding to key within c.m or nil if key doesn't exist
func (c *pdhQueryMap) GetQuery(key string) *pdhQuery {
	c.RLock()
	defer c.RUnlock()
	val, ok := c.m[key]
	if !ok {
		return nil
	}
	return val
}

// Add will place the key/value into c.m
func (c *pdhQueryMap) Add(key string, value *pdhQuery) {
	c.Lock()
	defer c.Unlock()
	c.m[key] = value
}

// Delete will delete the specified key from c.m
func (c *pdhQueryMap) Delete(key string) {
	c.Lock()
	defer c.Unlock()
	delete(c.m, key)
}

type QueryResult struct{
	Host string
	Query *pdhQuery
}

// IterateMap will send each key/value QueryResult contained in c.m to a returned channel
func (c *pdhQueryMap) IterateMap() <-chan QueryResult {
	i := make(chan QueryResult)

	go func() {
		defer func() {
			c.RUnlock()
			close(i)
		}()

		c.RLock()
		for k, v := range c.m {
			c.RUnlock()
			i <- QueryResult{k, v}
			c.RLock()
		}
	}()

	return i
}