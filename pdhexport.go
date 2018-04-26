package main

import (
	"time"
	"fmt"
	"os"
	"log"
	"github.com/lxn/win"
	"strings"
	"bufio"
	"flag"
	"net/http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sync"
	"unsafe"
	"regexp"
)

var (
	addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
	wg = sync.WaitGroup{}
	promMetrics = map[string]prometheus.Gauge{}
)

//type PromPdhCounter struct {
//	Counter string
//	PdhCounterHandle win.PDH_HCOUNTER
//	Instances map[string]prometheus.Gauge
//}

// The original version of ReadPerformanceCounter() taken from
// https://github.com/mavlyutov/golagraphite/blob/c41f6d55913190684b277dc7544ee491c9566fdc/perfcounters_windows.go
//func (p *PromPdhCounter) CollectCounter(done chan struct{}) error {
//	fmt.Printf("InitializeInstances %s\n", p.Counter)
//
//	var queryHandle win.PDH_HQUERY
//	var counterHandle win.PDH_HCOUNTER
//
//	ret := win.PdhOpenQuery(0, 0, &queryHandle)
//	if ret != win.ERROR_SUCCESS {
//		return errors.New("Unable to open query through DLL call")
//	}
//
//	// test path
//	ret = win.PdhValidatePath(p.Counter)
//	if ret == win.PDH_CSTATUS_BAD_COUNTERNAME {
//		return errors.New("Unable to fetch counter (this is unexpected)")
//	}
//
//	ret = win.PdhAddEnglishCounter(queryHandle, p.Counter, 0, &counterHandle)
//	if ret != win.ERROR_SUCCESS {
//		return errors.New(fmt.Sprintf("Unable to process counter. Error code is %x\n", ret))
//	}
//
//	ret = win.PdhCollectQueryData(queryHandle)
//	if ret != win.ERROR_SUCCESS {
//		return errors.New(fmt.Sprintf("Got an error: 0x%x\n", ret))
//	}
//
//	for {
//		ret = win.PdhCollectQueryData(queryHandle)
//		if ret == win.ERROR_SUCCESS {
//			var bufSize uint32
//			var bufCount uint32
//			var size= uint32(unsafe.Sizeof(win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE{}))
//			var emptyBuf [1]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE // need at least 1 addressable null ptr.
//
//			ret = win.PdhGetFormattedCounterArrayDouble(counterHandle, &bufSize, &bufCount, &emptyBuf[0])
//			if ret == win.PDH_MORE_DATA {
//				filledBuf := make([]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE, bufCount*size)
//				ret = win.PdhGetFormattedCounterArrayDouble(counterHandle, &bufSize, &bufCount, &filledBuf[0])
//				if ret == win.ERROR_SUCCESS {
//					for i := 0; i < int(bufCount); i++ {
//						c := filledBuf[i]
//						s := win.UTF16PtrToString(c.SzName)
//
//						if val, ok := p.Instances[s]; ok {
//							val.Set(c.FmtValue.DoubleValue)
//
//							// uncomment this line to have new values printed to console
//							//fmt.Printf("%s[%s] : %v\n", p.Counter, s, c.FmtValue.DoubleValue)
//						} else {
//							if g, err := counterToPrometheusGauge(p.Counter, s); err == nil {
//								p.Instances[s] = prometheus.NewGauge(g)
//								prometheus.MustRegister(p.Instances[s])
//							} else {
//								return err
//							}
//						}
//					}
//				}
//			}
//		} else {
//			fmt.Printf("failed to obtain instances for: %s\n", p.Counter)
//		}
//
//		select{
//		case <- done:
//			for k,v := range p.Instances {
//				prometheus.Unregister(v)
//				fmt.Printf("unregistered %s[%s]\n", p.Counter, k)
//			}
//			return nil
//		case <- time.After(p.CollectionInterval):
//			// do nothing
//		}
//
//		win.PdhCloseQuery(queryHandle)
//	}
//
//	return nil
//}

func main() {
	flag.Parse()

	const COUNTERS_FILE= "counters.txt"
	countersFileChangedChan := make(chan struct{})
	errorsChan := make(chan error)
	done := make(chan struct{})

	// Watch for the counters.txt file to change
	go watchFile(COUNTERS_FILE, countersFileChangedChan, errorsChan)

	go func() {
		for {
			select {
			case <-countersFileChangedChan:
				fmt.Printf("%s changed\n", COUNTERS_FILE)

				// Tell all the collectors to stop collection and wait for them all to shutdown
				close(done)
				wg.Wait()

				// reinitialize the done channel to allow collection to restart
				done = make(chan struct{})

				countersChannel := make(chan string)
				go readCounterConfigFile(COUNTERS_FILE, countersChannel)
				go processCounters(countersChannel)
			case err := <-errorsChan:
				fmt.Printf("error occurred while watching %s -> %s\n", COUNTERS_FILE, err)
				return
			}

			time.Sleep(5 * time.Second)
		}
	}()

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// watchFile watches the file located at filePath for changes and sends a message through
// the channel fileChangedChan when the file has been changed. If an error occurs, it will be
// sent through the channel errorsChan.
func watchFile(filePath string, fileChangedChan chan struct{}, errorsChan chan error) {
	var initialStat os.FileInfo
	for {
		stat, err := os.Stat(filePath)
		if err != nil {
			errorsChan <- err
			return
		}

		if initialStat == nil || stat.Size() != initialStat.Size() || stat.ModTime() != initialStat.ModTime() {
			initialStat = stat
			fileChangedChan <- struct{}{}
		}

		time.Sleep(1 * time.Second)
	}
}

// readCounterConfigFile reads in performance counter strings from file. Each line within
// the file should contain exactly 1 performance counter.
// Examples:
//		All instances of a single counter: \Processor(*)\% Processor Time
//		A single instance counter: \Processor(_Total)\% Processor Time
//		A Remote counter: \\myhost\Processor(*)\% Processor Time
func readCounterConfigFile(file string, countersChannel chan string) {
	f, err := os.Open(file)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		countersChannel <- scanner.Text()
	}
	if err := scanner.Err(); err != nil {
		fmt.Printf("error reading file (%s): %s\n", file, err)
	}

	close(countersChannel)
	fmt.Println("closed countersChannel")
}

// processCounters receives counters in countersChan and the processes them to be
// collected and published. The done channel can be used to stop collection of
// all initialized counters.
func processCounters(countersChannel chan string) {
	var queryHandle win.PDH_HQUERY
	counterHandles := map[string]*win.PDH_HCOUNTER{}

	ret := win.PdhOpenQuery(0, 0, &queryHandle)
	if ret != win.ERROR_SUCCESS {
		fmt.Printf("failed PdhOpenQuery, %x\n", ret)
	} else {
		for counter := range countersChannel {
			var c win.PDH_HCOUNTER
			ret = win.PdhValidatePath(counter)
			if ret == win.PDH_CSTATUS_BAD_COUNTERNAME {
				fmt.Printf("failed PdhValidatePath, %s, %x\n", counter, ret)
				continue
			}

			ret = win.PdhAddEnglishCounter(queryHandle, counter, 0, &c)
			if ret != win.ERROR_SUCCESS {
				if ret != win.PDH_CSTATUS_NO_OBJECT {
					fmt.Printf("failed PdhAddEnglishCounter, %s, %x\n", counter, ret)
				}
				continue
			}

			counterHandles[counter] = &c
		}

		ret = win.PdhCollectQueryData(queryHandle)
		if ret != win.ERROR_SUCCESS {
			fmt.Printf("failed PdhCollectQueryData, %x\n", ret)
		} else {
			for {
				ret := win.PdhCollectQueryData(queryHandle)
				if ret == win.ERROR_SUCCESS {
					for k, v := range counterHandles {
						var bufSize uint32
						var bufCount uint32
						var size= uint32(unsafe.Sizeof(win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE{}))
						var emptyBuf [1]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE // need at least 1 addressable null ptr.

						ret = win.PdhGetFormattedCounterArrayDouble(*v, &bufSize, &bufCount, &emptyBuf[0])
						if ret == win.PDH_MORE_DATA {
							filledBuf := make([]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE, bufCount*size)
							ret = win.PdhGetFormattedCounterArrayDouble(*v, &bufSize, &bufCount, &filledBuf[0])
							if ret == win.ERROR_SUCCESS {
								for i := 0; i < int(bufCount); i++ {
									c := filledBuf[i]
									s := win.UTF16PtrToString(c.SzName)

									if val, ok := promMetrics[k + s]; ok {
										val.Set(c.FmtValue.DoubleValue)

										// uncomment this line to have new values printed to console
										//fmt.Printf("%s[%s] : %v\n", p.Counter, s, c.FmtValue.DoubleValue)
									} else {
										if g, err := counterToPrometheusGauge(k, s); err == nil {
											promMetrics[k + s] = prometheus.NewGauge(g)
											prometheus.MustRegister(promMetrics[k + s])
										} else {
											fmt.Printf("failed counterToPrometheusGauge, %s, %s\n", k, err)
										}
									}
								}
							}
						}
					}
				}

				time.Sleep(5 * time.Second)
			}
		}
	}
}

// counterToPrometheusGauge converts a windows performance counter string into
// a prometheus Gauge. Prometheus Naming Conventions: https://prometheus.io/docs/concepts/data_model/
func counterToPrometheusGauge(counter, instance string) (prometheus.GaugeOpts, error) {
	// Replace known runes that occur in winpdh
	r := strings.NewReplacer(
		".", "_",
		"-", "_",
		" ", "_",
		"/","_",
		"%", "percent",
	)
	counter = r.Replace(counter)
	instance = r.Replace(instance)

	fields := strings.Split(counter, "\\")
	hostname := "localhost"
	catIndex := 1
	valIndex := 2
	category := ""

	// If the string contains a hostname
	if len(fields) == 5 {
		hostname = fields[2]
		catIndex = 3
		valIndex = 4
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

	// Use this regex to replace any invalid characters that weren't accounted for already
	reg, err := regexp.Compile("[^a-zA-Z0-9_:]")
	if err != nil {
		return prometheus.GaugeOpts{}, err
	}

	return prometheus.GaugeOpts{
		ConstLabels: prometheus.Labels{"hostname": string(reg.ReplaceAll([]byte(hostname),[]byte(""))), "pdhcategory": string(reg.ReplaceAll([]byte(category),[]byte(""))), "pdhinstance": string(reg.ReplaceAll([]byte(instance),[]byte("")))},
		Help: "windows performance counter",
		Name: string(reg.ReplaceAll([]byte(fields[valIndex]),[]byte(""))),
		Namespace:"winpdh",
	}, nil
}