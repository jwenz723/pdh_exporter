package main

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"github.com/prometheus/client_golang/prometheus"
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
	Counters []PdhCounter
	Done 	 chan struct{} // When this channel is closed, the collected Counters are unregistered from Prometheus and collection is stopped
	Host     string
	Interval time.Duration
	PdhQHandle win.PDH_HQUERY
	PdhCHandles map[string]*win.PDH_HCOUNTER
	PromCollectors map[string]prometheus.Gauge
	PromWaitGroup sync.WaitGroup
}

func (p *PdhCounterSet) Collect(doneChan chan struct{}) {
	fmt.Printf("%s: start Collect\n", p.Host)

	p.PdhCHandles = map[string]*win.PDH_HCOUNTER{}

	ret := win.PdhOpenQuery(0, 0, &p.PdhQHandle)
	if ret != win.ERROR_SUCCESS {
		fmt.Printf("%s: failed PdhOpenQuery, %x\n", p.Host, ret)
	} else {
		for _, c := range p.Counters {
			counter := fmt.Sprintf("\\\\%s%s", p.Host, c.Path)
			var c win.PDH_HCOUNTER
			ret = win.PdhValidatePath(counter)
			if ret == win.PDH_CSTATUS_BAD_COUNTERNAME {
				fmt.Printf("%s: failed PdhValidatePath, %s, %x\n", p.Host, counter, ret)
				continue
			}

			ret = win.PdhAddEnglishCounter(p.PdhQHandle, counter, 0, &c)
			if ret != win.ERROR_SUCCESS {
				if ret != win.PDH_CSTATUS_NO_OBJECT {
					fmt.Printf("%s: failed PdhAddEnglishCounter, %s, %x\n", p.Host, counter, ret)
				}
				continue
			}

			p.PdhCHandles[counter] = &c
		}

		ret = win.PdhCollectQueryData(p.PdhQHandle)
		if ret != win.ERROR_SUCCESS {
			fmt.Printf("%s: failed PdhCollectQueryData, %x\n", p.Host, ret)
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
										val.Set(c.FmtValue.DoubleValue)

										// uncomment this line to have new values printed to console
										//fmt.Printf("%s[%s] : %v\n", p.Counter, s, c.FmtValue.DoubleValue)
									} else {
										if g, err := counterToPrometheusGauge(k, s); err == nil {
											p.PromCollectors[k+s] = prometheus.NewGauge(g)
											if err = prometheus.Register(p.PromCollectors[k+s]); err != nil {
												fmt.Printf("%s: failed to register with prometheus: %s - %s\n", p.Host, k, s)
												delete(p.PromCollectors, k+s)
												close(p.Done)
												return
											}
											p.PromWaitGroup.Add(1)
										} else {
											fmt.Printf("%s: failed counterToPrometheusGauge, %s, %s\n", p.Host, k, err)
										}
									}
								}
							}
						}
					}
				}

				select{
				case <- doneChan:
					fmt.Printf("%s: received global done\n", p.Host)
					p.UnregisterPrometheusCollectors()
					return
				case <- p.Done:
					fmt.Printf("%s: received instance done\n", p.Host)
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
			fmt.Printf("%s: failed to unregister %s\n", p.Host, k)
		} else {
			delete(p.PromCollectors, k)
			p.PromWaitGroup.Done()
			fmt.Printf("%s: unregistered %s\n", p.Host, k)
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