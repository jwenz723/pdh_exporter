package main

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

type PdhCounter struct {
	Path string
	Multiplier float64
}

// TestEquivalence will test if a is equal to p
func (p *PdhCounter) TestEquivalence(a *PdhCounter) bool {
	return p.Path == a.Path && p.Multiplier == a.Multiplier
}

// UnmarshalYAML will be called any time the PdhCounter struct is being
// unmarshaled. This function is being implemented so that PdhCounter can
// have a default value set for Multiplier
func (p *PdhCounter) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawPdhCounter PdhCounter
	raw := rawPdhCounter{Multiplier:1.0} // Set default multiplier value of 1.0
	if err := unmarshal(&raw); err != nil {
		return err
	}

	*p = PdhCounter(raw)
	return nil
}

// PdhCounterSet defines a PdhCounter set to be collected on a single Host
type PdhCounterSet struct {
	completedInitialization bool
	Counters []PdhCounter
	Done 	 chan struct{} // When this channel is closed, the collected Counters are unregistered from Prometheus and collection is stopped
	Host     string
	Interval time.Duration
	PdhQHandle win.PDH_HQUERY
	PdhCHandles map[string]*win.PDH_HCOUNTER
	PromCollectors map[string]prometheus.Gauge
	PromWaitGroup sync.WaitGroup
}

// Collect will start the collection for the defined Host and Counters in p
func (p *PdhCounterSet) Collect() {
	log.WithFields(log.Fields{
		"host": p.Host,
	}).Info("%s: start Collect()")
	//fmt.Printf("%s: start Collect()\n", p.Host)

	p.PdhCHandles = map[string]*win.PDH_HCOUNTER{}

	ret := win.PdhOpenQuery(0, 0, &p.PdhQHandle)
	if ret != win.ERROR_SUCCESS {
		log.WithFields(log.Fields{
			"host": p.Host,
			"PDHError": fmt.Sprintf("%x",ret),
		}).Error("failed PdhOpenQuery")
		//fmt.Printf("%s: failed PdhOpenQuery() -> %x\n", p.Host, ret)
	} else {
		for _, c := range p.Counters {
			counter := fmt.Sprintf("\\\\%s%s", p.Host, c.Path)
			var c win.PDH_HCOUNTER
			ret = win.PdhValidatePath(counter)
			if ret == win.PDH_CSTATUS_BAD_COUNTERNAME {
				log.WithFields(log.Fields{
					"host": p.Host,
					"counter": counter,
					"PDHError": fmt.Sprintf("%x",ret),
				}).Error("failed PdhValidatePath")
				//fmt.Printf("%s: failed PdhValidatePath() for %s -> %x\n", p.Host, counter, ret)
				continue
			}

			ret = win.PdhAddEnglishCounter(p.PdhQHandle, counter, 0, &c)
			if ret != win.ERROR_SUCCESS {
				if ret != win.PDH_CSTATUS_NO_OBJECT {
					log.WithFields(log.Fields{
						"counter": counter,
						"host": p.Host,
						"PDHError": fmt.Sprintf("%x",ret),
					}).Error("failed PdhAddEnglishCounter")
					//fmt.Printf("%s: failed PdhAddEnglishCounter() for %s -> %x", p.Host, counter, ret)
				}
				continue
			}

			p.PdhCHandles[counter] = &c
		}

		ret = win.PdhCollectQueryData(p.PdhQHandle)
		if ret != win.ERROR_SUCCESS {
			log.WithFields(log.Fields{
				"host": p.Host,
				"PDHError": fmt.Sprintf("%x",fmt.Sprintf("%x",ret)),
			}).Error("failed PdhCollectQueryData")
			//fmt.Printf("%s: failed PdhCollectQueryData() -> %x\n", p.Host, ret)
		} else {
			p.PromCollectors = map[string]prometheus.Gauge{}
			p.PromWaitGroup = sync.WaitGroup{}

			for {
				ret := win.PdhCollectQueryData(p.PdhQHandle)
				if ret == win.ERROR_SUCCESS {
					for k, v := range p.PdhCHandles {
						var bufSize uint32
						var bufCount uint32
						var size = uint32(unsafe.Sizeof(win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE{}))
						var emptyBuf [1]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE // need at least 1 addressable null ptr.

						ret = win.PdhGetFormattedCounterArrayDouble(*v, &bufSize, &bufCount, &emptyBuf[0])
						if ret == win.PDH_MORE_DATA {
							filledBuf := make([]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE, bufCount*size)
							ret = win.PdhGetFormattedCounterArrayDouble(*v, &bufSize, &bufCount, &filledBuf[0])
							if ret == win.ERROR_SUCCESS {
								for i := 0; i < int(bufCount); i++ {
									c := filledBuf[i]
									s := win.UTF16PtrToString(c.SzName)

									if val, ok := p.PromCollectors[k+s]; ok {
										// TODO: implement counter multiplier
										val.Set(c.FmtValue.DoubleValue)
									} else {
										if g, err := counterToPrometheusGauge(k, s); err == nil {
											p.PromCollectors[k+s] = prometheus.NewGauge(g)

											if err = prometheus.Register(p.PromCollectors[k+s]); err != nil {
												if e, ok := err.(prometheus.AlreadyRegisteredError); ok {
													log.WithFields(log.Fields{
														"counter": k,
														"PDHInstance": s,
														"host": p.Host,
														"error": e,
													}).Warnf("Collector already registered with prometheus")
													//fmt.Printf("%s: Collector already registered -> %v\n", p.Host, e)
												} else {
													log.WithFields(log.Fields{
														"counter": k,
														"PDHInstance": s,
														"host": p.Host,
														"error": err,
													}).Error("failed to register with prometheus")
													//fmt.Printf("%s: failed to register with prometheus -> %v\n", p.Host, err)
													close(p.Done)
													goto teardown
												}
											} else {
												p.PromWaitGroup.Add(1)
												log.WithFields(log.Fields{
													"counter": k,
													"PDHInstance": s,
													"host": p.Host,
												}).Info("Collector registered with prometheus")
											}
										} else {
											log.WithFields(log.Fields{
												"counter": k,
												"host": p.Host,
												"error": err,
											}).Error("failed counterToPrometheusGauge")
											//fmt.Printf("%s: failed counterToPrometheusGauge -> %v\n", p.Host, err)
										}
									}
								}
							}
						}
					}
				}

				if !p.completedInitialization {
					p.completedInitialization = true
					log.WithFields(log.Fields{
						"host": p.Host,
					}).Info("completed Collect() initialization")
					//fmt.Printf("%s: completed Collect() initialization\n", p.Host)
				} else {
					log.WithFields(log.Fields{
						"host": p.Host,
					}).Debug("completed Collect() iteration")
					//fmt.Printf("%s: completed Collect() iteration\n", p.Host)
				}

				teardown:
				select{
				case <- p.Done:
					log.WithFields(log.Fields{
						"host": p.Host,
					}).Info("received instance done")
					//fmt.Printf("%s: received instance done\n", p.Host)
					p.UnregisterPrometheusCollectors()
					return
				case <- time.After(p.Interval):
					// do nothing
				}
			}
		}
	}
}

// UnregisterPrometheusCollectors unregisters all prometheus collector instances in use by p
func (p *PdhCounterSet) UnregisterPrometheusCollectors() {
	for k, v := range p.PromCollectors {
		if b := prometheus.Unregister(v); !b {
			log.WithFields(log.Fields{
				"collector": k,
				"host": p.Host,
			}).Error("failed to unregister Prometheus Collector\n")
			//fmt.Printf("%s: failed to unregister Prometheus Collector for %s\n", p.Host, k)
		} else {
			delete(p.PromCollectors, k)
			p.PromWaitGroup.Done()
			log.WithFields(log.Fields{
				"collector": k,
				"host": p.Host,
			}).Debug("unregistered Prometheus Collector")
			//fmt.Printf("%s: unregistered Prometheus Collector for %s\n", p.Host, k)
		}
	}
}

// TestEquivalence will test if a is equivalent to p
func (p *PdhCounterSet) TestEquivalence(a *PdhCounterSet) bool {
	if p.Host != a.Host || p.Interval != a.Interval || len(p.Counters) != len(a.Counters) {
		return false
	}

	for i := range p.Counters {
		if !p.Counters[i].TestEquivalence(&a.Counters[i]) {
			return false
		}
	}

	return true
}