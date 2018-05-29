package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
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

// StopCollect shuts down the collection that was started by StartCollect()
// and waits for all prometheus collectors to be unregistered.
func (p *PdhCounterSet) StopCollect() {
	// stop the old collection set
	close(p.Done)

	// Wait until all Prometheus Collectors have been unregistered to prevent clashing with registration of the new Collectors
	p.PromWaitGroup.Wait()
}

// StartCollect will start the collection for the defined Host and Counters in p
func (p *PdhCounterSet) StartCollect() error {
	defer p.UnregisterPrometheusCollectors()

	log.WithFields(log.Fields{
		"host": p.Host,
	}).Info("start StartCollect()")

	// Initialize basics of prometheus
	p.PromCollectors = map[string]prometheus.Gauge{}
	p.PromWaitGroup = sync.WaitGroup{}

	// Add a collector to track how many pdh counters fail to collect
	g := prometheus.GaugeOpts{
		ConstLabels:prometheus.Labels{"hostname":p.Host},
		Help: "The number of counters that failed to initialize",
		Name: "failed_collectors",
		Namespace:"winpdh",
	}
	if err := p.AddPrometheusCollector("FailedCollectors", g); err != nil {
		log.WithFields(log.Fields{
			"host": p.Host,
		}).Errorf("failed to add 'FailedCollectors' prometheus collector -> %s", err)
		return err
	}

	p.PdhCHandles = map[string]*win.PDH_HCOUNTER{}

	ret := win.PdhOpenQuery(0, 0, &p.PdhQHandle)
	if ret != win.ERROR_SUCCESS {
		log.WithFields(log.Fields{
			"host": p.Host,
			"PDHError": fmt.Sprintf("%x",ret),
		}).Error("failed PdhOpenQuery")
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
				p.PromCollectors["FailedCollectors"].Add(1)
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
				}
				p.PromCollectors["FailedCollectors"].Add(1)
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
		} else {
			loop:
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
											if err := p.AddPrometheusCollector(k+s, g); err != nil {
												if e, ok := err.(prometheus.AlreadyRegisteredError); ok {
													log.WithFields(log.Fields{
														"counter": k,
														"PDHInstance": s,
														"host": p.Host,
														"error": e,
													}).Warnf("Collector already registered with prometheus")
												} else {
													log.WithFields(log.Fields{
														"counter": k,
														"PDHInstance": s,
														"host": p.Host,
														"error": err,
													}).Error("failed to register with prometheus")
													close(p.Done)
													return err
												}
											} else {
												log.WithFields(log.Fields{
													"counter": k,
													"PDHInstance": s,
													"host": p.Host,
												}).Debug("Collector registered with prometheus")
											}
										} else {
											log.WithFields(log.Fields{
												"counter": k,
												"host": p.Host,
												"error": err,
											}).Error("failed counterToPrometheusGauge")
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
					}).Info("completed StartCollect() initialization")
				} else {
					log.WithFields(log.Fields{
						"host": p.Host,
					}).Debug("completed StartCollect() iteration")
				}

				select{
				case <- p.Done:
					log.WithFields(log.Fields{
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

// AddPrometheusCollector adds a new gauge into PromCollectors and updates the number in PromWaitGroup
func (p *PdhCounterSet) AddPrometheusCollector(key string, g prometheus.GaugeOpts) error {
	p.PromCollectors[key] = prometheus.NewGauge(g)
	if err := prometheus.Register(p.PromCollectors[key]); err != nil {
		return err
	} else {
		p.PromWaitGroup.Add(1)
		return nil
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
		} else {
			delete(p.PromCollectors, k)
			p.PromWaitGroup.Done()
			log.WithFields(log.Fields{
				"collector": k,
				"host": p.Host,
			}).Debug("unregistered Prometheus Collector")
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

// counterToPrometheusGauge converts a windows performance counter string into
// a prometheus Gauge.
//
// According to https://prometheus.io/docs/concepts/data_model/
// 		- Prometheus Metric Names must match: [a-zA-Z_:][a-zA-Z0-9_:]*
//		- Prometheus Label Restrictions:
// 			- Label names must match: [a-zA-Z_][a-zA-Z0-9_]*
//			- Label values: may contain any Unicode characters
//
// Additional Prometheus Metric/Label naming conventions: https://prometheus.io/docs/practices/naming/
func counterToPrometheusGauge(counter, instance string) (prometheus.GaugeOpts, error) {
	fields := strings.Split(counter, "\\")
	var hostname string
	var catIndex int
	var valIndex int
	var category string

	// If the string contains a hostname
	if len(fields) == 5 {
		hostname = fields[2]
		catIndex = 3
		valIndex = 4
	} else if len(fields) == 3 {
		hostname = "localhost"
		catIndex = 1
		valIndex = 2
	} else {
		return prometheus.GaugeOpts{}, errors.New("Unknown number of fields in counter: " + counter)
	}

	if strings.Contains(fields[catIndex], "(") {
		catFields := strings.Split(fields[catIndex], "(")
		category = catFields[0]
		i := strings.TrimSuffix(catFields[1], ")")
		if i != "*" {
			instance = i
		}
	} else {
		category = fields[catIndex]
	}

	// Replace known runes that occur in winpdh
	r := strings.NewReplacer(
		".", "_",
		"-", "_",
		" ", "_",
		"/","_",
		"%", "percent",
	)
	counterName := r.Replace(fields[valIndex])
	instance = r.Replace(instance)

	// Use this regex to replace any invalid characters that weren't accounted for already
	reg, err := regexp.Compile("[^a-zA-Z0-9_:]")
	if err != nil {
		return prometheus.GaugeOpts{}, err
	}

	category = string(reg.ReplaceAll([]byte(category),[]byte("")))
	instance = string(reg.ReplaceAll([]byte(instance),[]byte("")))

	return prometheus.GaugeOpts{
		ConstLabels: prometheus.Labels{"hostname": hostname, "pdhcategory": category, "pdhinstance": instance},
		Help: "windows performance counter",
		Name: string(reg.ReplaceAll([]byte(counterName),[]byte(""))),
		Namespace:"winpdh",
	}, nil
}