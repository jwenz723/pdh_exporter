package main

import (
	"time"
	"fmt"
	"os"
	"log"
	"github.com/lxn/win"
	"github.com/marpaia/graphite-golang"
	//"github.com/fsnotify/fsnotify"
	"unsafe"
	"errors"
	"strings"
	"regexp"
	"bufio"
	"flag"
	"net/http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
)

func init() {

}

func main() {
	flag.Parse()

	//start := time.Now()
	//
	//retrieveChannel := make(chan *HostPerfCounter)
	//insertChannel := make(chan *InsertValue)
	//doneChannel := make(chan bool)
	//
	//go load(retrieveChannel)
	//go retrieve(retrieveChannel, insertChannel)
	//go insert(insertChannel, doneChannel)
	//
	//fmt.Println(<- doneChannel)
	//fmt.Printf("Processed all records in %v\n", time.Since(start))


	//if ch, err := ReadPerformanceCounter("\\Processor(_Total)\\% Processor Time", 5); err != nil {
	//	fmt.Printf("error: %s", err)
	//} else {
	//	for {
	//		fmt.Println(<-ch)
	//	}
	//}

	//countersFile := "counters.txt"
	//
	//watcher, err := fsnotify.NewWatcher()
	//if err != nil {
	//	log.Fatal(err)
	//}
	//defer watcher.Close()
	//
	//done := make(chan bool)
	//go func() {
	//	for {
	//		select {
	//			case event := <-watcher.Events:
	//				log.Println("event:", event)
	//				if event.Op&fsnotify.Write == fsnotify.Write {
	//					log.Println("modified file:", event.Name)
	//
	//					countersChannel := make(chan string)
	//					go readCounterConfigFile(event.Name, countersChannel)
	//
	//					//for {
	//					//	select {
	//					//		case counter := <- countersChannel:
	//					//			log.Println("Received counter:", counter)
	//					//	}
	//					//}
	//				}
	//			case err := <-watcher.Errors:
	//				log.Println("error:", err)
	//				done <- true
	//		}
	//	}
	//}()
	//
	//err = watcher.Add(countersFile)
	//if err != nil {
	//	log.Fatal(err)
	//}
	//<-done

	const COUNTERS_FILE = "counters.txt"
	//countersFileChangedChan := make(chan bool)
	//errorsChan := make(chan error)

	//doneChan := make(chan bool)

	// Watch for the counters.txt file to change
	//go watchFile(COUNTERS_FILE, countersFileChangedChan, errorsChan)

	//for {
	//	select {
	//		case <-countersFileChangedChan:
				fmt.Println("counters file changed")

				countersChannel := make(chan string)
				go readCounterConfigFile(COUNTERS_FILE, countersChannel)
				go processCounters(countersChannel)

		//	case error := <- errorsChan:
		//		fmt.Println("error occurred while reading file: ", error)
		//}
		//
		//time.Sleep(1 * time.Second)
	//}

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func watchFile(filePath string, fileChangedChan chan bool, errorsChan chan error) {
	initialStat, err := os.Stat(filePath)
	if err != nil {
		errorsChan <- err
		return
	}

	for {
		stat, err := os.Stat(filePath)
		if err != nil {
			errorsChan <- err
			return
		}

		if stat.Size() != initialStat.Size() || stat.ModTime() != initialStat.ModTime() {
			initialStat = stat
			fileChangedChan <- true
		}

		time.Sleep(1 * time.Second)
	}
}

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

func processCounters(countersChan chan string) {
	for c := range countersChan {
		if c != "" {
			fmt.Printf("initializing counter: %s\n", c)
			go func(counter string) {
				if ch, err := ReadPerformanceCounter(counter, 1); err != nil {
					fmt.Printf("error: %s", err)
				} else {
					for {
						fmt.Println(<-ch)
					}
				}
			}(c)
		}
	}

	fmt.Println("processed all counters")
}

// The original version of ReadPerformanceCounter()
//func ReadPerformanceCounter(counter string, sleepInterval int) (chan []graphite.Metric, error) {
//
//	var queryHandle win.PDH_HQUERY
//	var counterHandle win.PDH_HCOUNTER
//
//	ret := win.PdhOpenQuery(0, 0, &queryHandle)
//	if ret != win.ERROR_SUCCESS {
//		return nil, errors.New("Unable to open query through DLL call")
//	}
//
//	// test path
//	ret = win.PdhValidatePath(counter)
//	if ret == win.PDH_CSTATUS_BAD_COUNTERNAME {
//		return nil, errors.New("Unable to fetch counter (this is unexpected)")
//	}
//
//	ret = win.PdhAddEnglishCounter(queryHandle, counter, 0, &counterHandle)
//	if ret != win.ERROR_SUCCESS {
//		return nil, errors.New(fmt.Sprintf("Unable to add process counter. Error code is %x\n", ret))
//	}
//
//	ret = win.PdhCollectQueryData(queryHandle)
//	if ret != win.ERROR_SUCCESS {
//		return nil, errors.New(fmt.Sprintf("Got an error: 0x%x\n", ret))
//	}
//
//	out := make(chan []graphite.Metric)
//
//	go func() {
//		for {
//			ret = win.PdhCollectQueryData(queryHandle)
//			if ret == win.ERROR_SUCCESS {
//
//				var metric []graphite.Metric
//
//				var bufSize uint32
//				var bufCount uint32
//				var size uint32 = uint32(unsafe.Sizeof(win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE{}))
//				var emptyBuf [1]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE // need at least 1 addressable null ptr.
//
//				ret = win.PdhGetFormattedCounterArrayDouble(counterHandle, &bufSize, &bufCount, &emptyBuf[0])
//				if ret == win.PDH_MORE_DATA {
//					filledBuf := make([]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE, bufCount*size)
//					ret = win.PdhGetFormattedCounterArrayDouble(counterHandle, &bufSize, &bufCount, &filledBuf[0])
//					if ret == win.ERROR_SUCCESS {
//						for i := 0; i < int(bufCount); i++ {
//							c := filledBuf[i]
//							s := win.UTF16PtrToString(c.SzName)
//
//							metricName := NormalizeMetricName(counter)
//							if len(s) > 0 {
//								metricName = fmt.Sprintf("%s.%s", NormalizeMetricName(counter), NormalizeMetricName(s))
//							}
//
//							metric = append(metric, graphite.Metric{
//								metricName,
//								fmt.Sprintf("%v", c.FmtValue.DoubleValue),
//								time.Now().Unix()})
//						}
//					}
//				}
//				out <- metric
//			}
//
//			time.Sleep(time.Duration(sleepInterval) * time.Second)
//		}
//	}()
//
//	return out, nil
//
//}

func ReadPerformanceCounter(counter string, sleepInterval int) (chan []graphite.Metric, error) {

	var queryHandle win.PDH_HQUERY
	var counterHandle win.PDH_HCOUNTER

	ret := win.PdhOpenQuery(0, 0, &queryHandle)
	if ret != win.ERROR_SUCCESS {
		return nil, errors.New("Unable to open query through DLL call")
	}

	// test path
	ret = win.PdhValidatePath(counter)
	if ret == win.PDH_CSTATUS_BAD_COUNTERNAME {
		return nil, errors.New("Unable to fetch counter (this is unexpected)")
	}

	ret = win.PdhAddEnglishCounter(queryHandle, counter, 0, &counterHandle)
	if ret != win.ERROR_SUCCESS {
		return nil, errors.New(fmt.Sprintf("Unable to process counter. Error code is %x\n", ret))
	}

	ret = win.PdhCollectQueryData(queryHandle)
	if ret != win.ERROR_SUCCESS {
		return nil, errors.New(fmt.Sprintf("Got an error: 0x%x\n", ret))
	}

	out := make(chan []graphite.Metric)

	go func() {
		promCounters := map[string]prometheus.Gauge{}
		for {
			ret = win.PdhCollectQueryData(queryHandle)
			if ret == win.ERROR_SUCCESS {

				var metric []graphite.Metric

				var bufSize uint32
				var bufCount uint32
				var size = uint32(unsafe.Sizeof(win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE{}))
				var emptyBuf [1]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE // need at least 1 addressable null ptr.

				ret = win.PdhGetFormattedCounterArrayDouble(counterHandle, &bufSize, &bufCount, &emptyBuf[0])
				if ret == win.PDH_MORE_DATA {
					filledBuf := make([]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE, bufCount*size)
					ret = win.PdhGetFormattedCounterArrayDouble(counterHandle, &bufSize, &bufCount, &filledBuf[0])
					if ret == win.ERROR_SUCCESS {
						for i := 0; i < int(bufCount); i++ {
							c := filledBuf[i]
							s := win.UTF16PtrToString(c.SzName)

							metricName := normalizePerfCounterMetricName(counter)
							if len(s) > 0 {
								metricName = fmt.Sprintf("%s.%s", metricName, normalizePerfCounterMetricName(s))
							}

							metric = append(metric, graphite.Metric{
								metricName,
								fmt.Sprintf("%v", c.FmtValue.DoubleValue),
								time.Now().Unix()})

							if val, ok := promCounters[s]; ok {
								val.Set(c.FmtValue.DoubleValue)
							} else {
								promCounters[s] = prometheus.NewGauge(counterToPrometheusGauge(counter, s))

								// Metrics have to be registered to be exposed:
								prometheus.MustRegister(promCounters[s])
							}
						}
					}
				}
				out <- metric
			} else {
				fmt.Println("failed to obtain value for: %s", counter)
			}

			time.Sleep(time.Duration(sleepInterval) * time.Second)
		}
	}()

	return out, nil

}

func normalizePerfCounterMetricName(rawName string) (normalizedName string) {

	normalizedName = rawName

	// thanks to Microsoft Windows,
	// we have performance counter metric like `\\Processor(_Total)\\% Processor Time`
	// which we need to convert to `processor_total.processor_time` see perfcounter_test.go for more beautiful examples
	r := strings.NewReplacer(
		".", "",
		"\\", ".",
		" ", "_",
	)
	normalizedName = r.Replace(normalizedName)

	normalizedName = NormalizeMetricName(normalizedName)
	return
}

func NormalizeMetricName(rawName string) (normalizedName string) {

	normalizedName = strings.ToLower(rawName)

	// remove trailing and leading non alphanumeric characters
	re1 := regexp.MustCompile(`(^[^a-z0-9]+)|([^a-z0-9]+$)`)
	normalizedName = re1.ReplaceAllString(normalizedName, "")

	// replace whitespaces with underscore
	re2 := regexp.MustCompile(`\s`)
	normalizedName = re2.ReplaceAllString(normalizedName, "_")

	// remove non alphanumeric characters except underscore and dot
	re3 := regexp.MustCompile(`[^a-z0-9._]`)
	normalizedName = re3.ReplaceAllString(normalizedName, "")

	return
}

func counterToPrometheusGauge(counter, instance string) prometheus.GaugeOpts {
	//re1 := regexp.MustCompile("(?![a-zA-Z_:][a-zA-Z0-9_:]*)")

	r := strings.NewReplacer(
		".", "_",
		"-", "_",
		" ", "_",
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

	return prometheus.GaugeOpts{
		ConstLabels: prometheus.Labels{"hostname": hostname, "category": category, "instance": instance},
		Help: "windows performance counter",
		Name: fields[valIndex],
		Namespace:"winpdh",
	}
}