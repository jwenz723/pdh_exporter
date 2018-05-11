package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
	config = flag.String("config", "config.yml", "Path to yml formatted config file.")
	wg = sync.WaitGroup{}

	// A map containing a reference to all PdhCounterSet that are being collected
	PCSCollectedSets = map[string]*PdhCounterSet{}
)

func main() {
	flag.Parse()

	configChan := make(chan struct{})
	errorsChan := make(chan error)
	doneChan := make(chan struct{})

	go watchFile(*config, configChan, errorsChan)

	go func() {
		for {
			select {
			case <- configChan:
				fmt.Printf("%s changed\n", *config)

				// Tell all the collectors to stop collection and wait for them all to shutdown
				//close(doneChan)
				//wg.Wait()

				// reinitialize the doneChan channel to allow collection to restart
				//doneChan = make(chan struct{})

				addPCSChan := make(chan PdhCounterSet)
				go ReadConfigFile(*config, addPCSChan)
				go processCounters(addPCSChan, doneChan)
			case err := <-errorsChan:
				fmt.Printf("error occurred while watching %s -> %s\n", *config, err)
				close(doneChan)
				wg.Wait()
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

// ReadConfigFile will parse the Yaml formatted file and pass along all PdhCounterSet that are new to addPCSChan
func ReadConfigFile(file string, addPCSChan chan PdhCounterSet) {
	newPCSCollectedSets := map[string]*PdhCounterSet{}

	config := NewConfig(file)
	defer func() {
		close(addPCSChan)
		fmt.Println("closed addPCSChan")
	}()

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
		if v, ok := PCSCollectedSets[hostName]; ok {
			if !v.TestEquivalence(cSet) {
				newSet = true

				// stop the old collection set
				close(v.Done)

				// Wait until all Prometheus Collectors have been unregistered to prevent clashing with registration of the new Collectors
				v.PromWaitGroup.Wait()
			} else {
				newSet = false
			}
		}

		if len(cSet.Counters) > 0 && newSet {
			addPCSChan <- *cSet
			fmt.Printf("%s: sent new PdhCounterSet\n", hostName)
		}
	}
}

// processCounters receives PdhCounterSet objects and the processes them to be
// collected and published. A single PDH Query will be created for each PdhCounterSet
// The done channel can be used to stop collection of all initialized counters.
func processCounters(addPCSChan chan PdhCounterSet, doneChan chan struct{}) {
	for cSet := range addPCSChan {
		go func(cSet PdhCounterSet) {
			PCSCollectedSets[cSet.Host] = &cSet
			cSet.Collect(doneChan)
			delete(PCSCollectedSets, cSet.Host)
			fmt.Printf("%s: finished Collect\n", cSet.Host)
		}(cSet)
	}
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