package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

var (
	addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
	config = flag.String("config", "config.yml", "Path to yml formatted config file.")
	logDirectory = flag.String("logDirectory", "logs", "Specify a directory where logs should be written to. Use \"\" to log to stdout.")
	logLevel = flag.String("logLevel", "info", "Use this flag to specify what level of logging you wish to have output. Available values: panic, fatal, error, warn, info, debug.")
	JSONOutput = flag.Bool("JSONOutput", false, "Use this flag to turn on json formatted logging.")

	// A map containing a reference to all PdhCounterSet that are being collected
	PCSCollectedSets = map[string]*PdhCounterSet{}

	// PCSCollectedSetsMux is used to insure safe writing to PCSCollectedSets
	PCSCollectedSetsMux = &sync.Mutex{}
)

func main() {
	flag.Parse()

	// Setup log path to log messages out to
	l, err := InitLogging(*logDirectory, *logLevel, *JSONOutput)
	if err != nil {
		log.Fatalf("error initializing log file -> %v\n", err)
	} else {
		defer func() {
			if err = l.Close(); err != nil {
				log.Fatalf("error closing log file -> %v\n", err)
			}
		}()
	}

	configChan := make(chan struct{})
	errorsChan := make(chan error)

	go watchFile(*config, configChan, errorsChan)

	go func() {
		for {
			select {
			case <- configChan:
				log.Infof("%s changed\n", *config)
				//addPCSChan := make(chan PdhCounterSet)
				go ReadConfigFile(*config)
				//go processCounters(addPCSChan)
			case err := <-errorsChan:
				log.Fatalf("error occurred while watching %s -> %s\n", *config, err)
			}

			time.Sleep(5 * time.Second)
		}
	}()

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// InitLogging is used to initialize all properties of the logrus
// logging library.
func InitLogging(logDirectory string, logLevel string, jsonOutput bool) (file *os.File, err error) {
	// if LogDirectory is "" then logging will just go to stdout
	if logDirectory != "" {
		if _, err = os.Stat(logDirectory); os.IsNotExist(err) {
			err := os.MkdirAll(logDirectory, 0777)
			if err != nil {
				return nil, err
			}

			// Chmod is needed because the permissions can't be set by the Mkdir function in Linux
			err = os.Chmod(logDirectory, 0777)
			if err != nil {
				return nil, err
			}
		}
		file, err = os.OpenFile(filepath.Join(logDirectory, fmt.Sprintf("%s%s", time.Now().Local().Format("20060102"), ".log")), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		log.SetOutput(file)
	} else {
		// Output to stdout instead of the default stderr
		log.SetOutput(os.Stdout)
	}

	logLevel = strings.ToLower(logLevel)

	if jsonOutput {
		log.SetFormatter(&log.JSONFormatter{})
	} else {
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	}

	l, err := log.ParseLevel(logLevel)
	if err != nil {
		log.SetLevel(log.InfoLevel)
	} else {
		log.SetLevel(l)
	}

	return file, nil
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

// ReadConfigFile will parse the Yaml formatted file and pass along all PdhCounterSet that are new to addPCSChan
func ReadConfigFile(file string) {
	newPCSCollectedSets := map[string]*PdhCounterSet{}

	config := NewConfig(file)
	//defer func() {
	//	close(addPCSChan)
	//	log.Debugf("closed addPCSChan\n")
	//}()

	for _, hostName := range config.Pdh_Counters.HostNames {
		// if the hostname has not already been processed
		if _, ok := newPCSCollectedSets[hostName]; !ok {
			newPCSCollectedSets[hostName] = &PdhCounterSet{
				Done: make(chan struct{}),
				Host:     hostName,
				Interval: time.Duration(config.Pdh_Counters.Interval) * time.Second,
			}

			// Add into cSet each PdhCounter that has a key that matches the hostname
			for k, v := range config.Pdh_Counters.Counters {
				if matched, _ := regexp.MatchString(k, hostName); matched {
					for _, counter := range v {
						newPCSCollectedSets[hostName].Counters = append(newPCSCollectedSets[hostName].Counters, counter)
					}
				}
			}
		}
	}

	// Figure out if any PdhCounterSet have been removed completely from collection
	for hostName, cSet := range PCSCollectedSets {
		if _, ok := newPCSCollectedSets[hostName]; !ok {
			// stop the old collection set
			close(cSet.Done)

			// Wait until all Prometheus Collectors have been unregistered to prevent clashing with registration of the new Collectors
			cSet.PromWaitGroup.Wait()
		}
	}

	// Figure out if any PdhCounterSet have been added or changed
	for hostName, cSet := range newPCSCollectedSets {
		newSet := true

		PCSCollectedSetsMux.Lock()
		v, ok := PCSCollectedSets[hostName]
		PCSCollectedSetsMux.Unlock()
		if ok {
			if !v.TestEquivalence(cSet) {
				newSet = true

				// stop the old collection set
				close(v.Done)

				// Wait until all Prometheus Collectors have been unregistered to prevent clashing with registration of the new Collectors
				cSet.PromWaitGroup.Wait()
			} else {
				newSet = false
			}
		}

		if len(cSet.Counters) > 0 && newSet {
			go func(cSet PdhCounterSet) {
				PCSCollectedSetsMux.Lock()
				PCSCollectedSets[cSet.Host] = &cSet
				PCSCollectedSetsMux.Unlock()

				cSet.Collect()

				PCSCollectedSetsMux.Lock()
				delete(PCSCollectedSets, cSet.Host)
				PCSCollectedSetsMux.Unlock()

				log.WithFields(log.Fields{
					"host": cSet.Host,
				}).Info("finished Collect()\n")
			}(*cSet)
			log.Debugf("%s: sent new PdhCounterSet\n", hostName)
		}
	}
}

// processCounters receives PdhCounterSet objects and the processes them to be
// collected and published. A single PDH Query will be created for each PdhCounterSet
// The done channel can be used to stop collection of all initialized counters.
//func processCounters(addPCSChan chan PdhCounterSet) {
//	for cSet := range addPCSChan {
//		go func(cSet PdhCounterSet) {
//			PCSCollectedSetsMux.Lock()
//			PCSCollectedSets[cSet.Host] = &cSet
//			PCSCollectedSetsMux.Unlock()
//
//			cSet.Collect()
//
//			PCSCollectedSetsMux.Lock()
//			delete(PCSCollectedSets, cSet.Host)
//			PCSCollectedSetsMux.Unlock()
//
//			log.WithFields(log.Fields{
//				"host": cSet.Host,
//			}).Info("finished Collect()\n")
//		}(cSet)
//	}
//}

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