// +build windows
package PdhCounter

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/jwenz723/win"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

const FAILED_COLLECTION_ATTEMPTS = 10 // The number of times a collector can fail before it is unregistered from prometheus

type PdhError struct {
	pdhErrorCode uint32
	message string
}

func (e PdhError) Error() string {
	return fmt.Sprintf("pdh error %x. %s", e.pdhErrorCode, e.message)
}

// pdhQuery defines a pdhCounter set to be collected on a single Host
type pdhQuery struct {
	counters                []*pdhCounter   // Contains all pdhCounter's to be collected
	Done                    chan struct{}   // When this channel is closed, the collected counters are unregistered from Prometheus and collection is stopped
	Host                    string          // Defines the host to collect counters from
	Interval                time.Duration   // Defines the interval at which collection of counters should be done
	IsLocalhost             bool            // Indicates that collection is being done for the host that is running this app
	PdhQHandle              win.PDH_HQUERY  // A handle to the PDH Query used for collecting counters
	PromWaitGroup           *sync.WaitGroup // This is used to track if PromCollectors still contains active collectors. This prevents the same collector from being registered with prometheus more than once when a pdhQuery is updated.
	failedCollector         prometheus.Gauge
	logger					*logrus.Logger	// a logger to which all log messages will be written to
}

// AddCounter adds a counter into pdhQuery
func (p *pdhQuery) AddCounter(c *pdhCounter) {
	p.counters = append(p.counters, c)
	p.PromWaitGroup.Add(1)
}

// NumCounters returns the number of counters within pdhQuery
func (p *pdhQuery) NumCounters() int {
	return len(p.counters)
}

// Start will start the collection for the defined pdhQuery
func (p *pdhQuery) Start() error {
	defer func() {
		p.unregisterPromCollectors()
	}()

	p.logger.WithFields(logrus.Fields{
		"host": p.Host,
	}).Info("starting pdhQuery")

	// Add a collector to track how many pdh counters fail to collect
	p.failedCollector = prometheus.NewGauge(prometheus.GaugeOpts{
		ConstLabels:prometheus.Labels{"hostname":p.Host},
		Help: "The number of counters that failed to initialize",
		Name: "failed_collectors",
		Namespace:"winpdh",
	})
	if err := prometheus.Register(p.failedCollector); err != nil {
		p.logger.WithFields(logrus.Fields{
			"host": p.Host,
		}).Errorf("failed to add 'FailedCollectors' prometheus collector -> %s", err)
		return err
	} else {
		p.PromWaitGroup.Add(1)
	}

	ret := win.PdhOpenQuery(0, 0, &p.PdhQHandle)
	if ret != win.ERROR_SUCCESS {
		return PdhError{ret, "failed PdhOpenQuery"}
	} else {
		for i := len(p.counters)-1; i >= 0; i-- {
			p.logger.WithFields(logrus.Fields{
				"counter": p.counters[i].Path.String(),
				"host": p.Host,
			}).Debug("initializing pdhCounter")
			var ch win.PDH_HCOUNTER
			ret = win.PdhValidatePath(p.counters[i].Path.String())
			if ret != win.ERROR_SUCCESS {
				p.logger.WithFields(logrus.Fields{
					"host": p.Host,
					"counter": p.counters[i].Path,
					"PDHError": fmt.Sprintf("%x",ret),
				}).Error("failed PdhValidatePath")
				p.failedCollector.Add(1)
				p.counters = p.counters[:i+copy(p.counters[i:], p.counters[i+1:])]
				continue
			}

			ret = win.PdhAddEnglishCounter(p.PdhQHandle, p.counters[i].Path.String(), 0, &ch)
			if ret != win.ERROR_SUCCESS {
				if ret != win.PDH_CSTATUS_NO_OBJECT {
					p.logger.WithFields(logrus.Fields{
						"counter": p.counters[i].Path,
						"host": p.Host,
						"PDHError": fmt.Sprintf("%x",ret),
					}).Error("failed PdhAddEnglishCounter")
				} else {
					p.logger.WithFields(logrus.Fields{
						"counter": p.counters[i].Path,
						"host": p.Host,
						"PDHError": fmt.Sprintf("%x",ret),
					}).Warn("failed PdhAddEnglishCounter, most likely because the counter doesn't exist.")
				}
				p.failedCollector.Add(1)
				p.counters = p.counters[:i+copy(p.counters[i:], p.counters[i+1:])]
				continue
			}

			p.counters[i].handle = &ch
		}

		if len(p.counters) == 0 {
			return fmt.Errorf("no data to collect: no valid counters")
		}

		ret = win.PdhCollectQueryData(p.PdhQHandle)
		if ret != win.ERROR_SUCCESS {
			return PdhError{ret, "failed PdhCollectQueryData"}
		} else {
		loop:
			for {
				ret := win.PdhCollectQueryData(p.PdhQHandle)
				if ret == win.ERROR_SUCCESS {
					for i, v := range p.counters {
						var bufSize uint32
						var bufCount uint32
						var size = uint32(unsafe.Sizeof(win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE{}))
						var emptyBuf [1]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE // need at least 1 addressable null ptr.

						ret = win.PdhGetFormattedCounterArrayDouble(*v.handle, &bufSize, &bufCount, &emptyBuf[0])
						if ret == win.PDH_MORE_DATA {
							filledBuf := make([]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE, bufCount*size)
							ret = win.PdhGetFormattedCounterArrayDouble(*v.handle, &bufSize, &bufCount, &filledBuf[0])
							if ret == win.ERROR_SUCCESS {
								for i := 0; i < int(bufCount); i++ {
									c := filledBuf[i]
									s := win.UTF16PtrToString(c.SzName)

									// TODO: figure out how to exclude s from being reported if it exists in the defined ExcludeCounters
									if val, ok := v.promCollectors[s]; ok {
										(*val).Set(c.FmtValue.DoubleValue)
										v.collectionFailures = 0
									} else {
										if v.collectionFailures == FAILED_COLLECTION_ATTEMPTS {
											p.failedCollector.Add(-1)
										}

										if g, err := v.counterToPrometheusGauge(s); err == nil {
											if err := v.registerPromCollector(s, g); err != nil {
												if e, ok := err.(prometheus.AlreadyRegisteredError); ok {
													p.logger.WithFields(logrus.Fields{
														"counter": v.Path.String(),
														"PDHInstance": s,
														"host": p.Host,
														"error": e,
													}).Warnf("Collector already registered with prometheus")
												} else {
													p.logger.WithFields(logrus.Fields{
														"counter": v.Path.String(),
														"PDHInstance": s,
														"host": p.Host,
														"error": err,
													}).Error("failed to register with prometheus")
													close(p.Done)
													return err
												}
											} else {
												p.logger.WithFields(logrus.Fields{
													"counter": v.Path.String(),
													"PDHInstance": s,
													"host": p.Host,
												}).Debug("Collector registered with prometheus")
											}
										} else {
											p.logger.WithFields(logrus.Fields{
												"counter": v.Path.String(),
												"host": p.Host,
												"error": err,
											}).Error("failed counterToPrometheusGauge")
										}
									}
								}
							} else {
								if v.collectionFailures < FAILED_COLLECTION_ATTEMPTS {
									p.logger.WithFields(logrus.Fields{
										"counter":  v.Path.String(),
										"host":     p.Host,
										"PDHError": fmt.Sprintf("%x", ret),
									}).Error("failed PdhGetFormattedCounterArrayDouble")

									v.collectionFailures++

									if v.collectionFailures == FAILED_COLLECTION_ATTEMPTS {
										p.failedCollector.Add(1)

										// stop reporting counter to prometheus
										p.counters[i].unregisterPromCollectors()

										p.logger.WithFields(logrus.Fields{
											"counter":  v.Path.String(),
											"host":     p.Host,
											"PDHError": fmt.Sprintf("%x", ret),
										}).Info("unregistering collector from prometheus due to 10 consecutive failed collection attempts.")
									}
								}
							}
						} else {
							if v.collectionFailures < FAILED_COLLECTION_ATTEMPTS {
								p.logger.WithFields(logrus.Fields{
									"counter": v.Path.String(),
									"host":    p.Host,
								}).Warn("No data exists for counter.")

								v.collectionFailures++

								if v.collectionFailures == FAILED_COLLECTION_ATTEMPTS {
									p.failedCollector.Add(1)

									// stop reporting counter to prometheus
									p.counters[i].unregisterPromCollectors()

									p.logger.WithFields(logrus.Fields{
										"counter":  v.Path.String(),
										"host":     p.Host,
										"PDHError": fmt.Sprintf("%x", ret),
									}).Info("unregistering collector from prometheus due to 10 consecutive failed collection attempts.")
								}
							}
						}
					}
				}

				select{
				case <- p.Done:
					p.logger.WithFields(logrus.Fields{
						"host": p.Host,
					}).Info("instance Done channel was closed")
					break loop // must specify name of loop or else it will just break out of select{}
				case <- time.After(p.Interval):
					// do nothing
				}
			}
		}
	}

	return nil
}

// Stop shuts down the collection that was started by Start()
// and waits for all prometheus collectors to be unregistered.
func (p *pdhQuery) Stop() {
	p.logger.WithFields(logrus.Fields{
		"host": p.Host,
	}).Info("stopping pdhQuery")

	// stop the old collection set
	close(p.Done)

	// Wait until all Prometheus Collectors have been unregistered to prevent clashing with registration of the new Collectors
	p.PromWaitGroup.Wait()
}

// unregisterPromCollectors will unregister all counters within pdhQuery from prometheus
func (p *pdhQuery) unregisterPromCollectors() {
	for i := range p.counters {
		p.counters[i].unregisterPromCollectors()
		p.PromWaitGroup.Done()
	}
	prometheus.Unregister(p.failedCollector)
	p.PromWaitGroup.Done()
}

// TestEquivalence will test if 2 pdhQuery are the same
func (p *pdhQuery) TestEquivalence(a *pdhQuery) bool {
	if p.Host != a.Host || p.Interval != a.Interval || len(p.counters) != len(a.counters) {
		return false
	}

	for i := range p.counters {
		if p.counters[i].Path != a.counters[i].Path {
			return false
		}
	}

	return true
}

// NewPdhQuery creates a new pdhQuery
func NewPdhQuery(host string, interval time.Duration, isLocalHost bool, logger *logrus.Logger) *pdhQuery {
	p := pdhQuery{
		Done:          make(chan struct{}),
		Host:          host,
		Interval:      interval,
		IsLocalhost:   isLocalHost,
		counters:      []*pdhCounter{},
		PromWaitGroup: &sync.WaitGroup{},
		logger:		   logger,
	}

	return &p
}