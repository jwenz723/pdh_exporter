package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"regexp"

	//"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jwenz723/pdhexport/PdhCounter"
	"github.com/kardianos/service"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

var (
	addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
	config = flag.String("config", "config.yml", "Fully qualified path to yml formatted config file.")
	logDirectory = flag.String("logDirectory", "logs", "Specify a directory where logs should be written to. Use \"\" to log to stdout.")
	logLevel = flag.String("logLevel", "info", "Use this flag to specify what level of logging you wish to have output. Available values: panic, fatal, error, warn, info, debug.")
	JSONOutput = flag.Bool("JSONOutput", false, "Use this flag to turn on json formatted logging.")
	svcFlag = flag.String("service", "", "Control the system service. Valid actions: start, stop, restart, install, uninstall")

	// A map containing a reference to all PdhHostSet that are being collected
	PCSCollectedSets = map[string]*PdhCounter.PdhHostSet{}

	// PCSCollectedSetsMux is used to insure safe writing to PCSCollectedSets
	PCSCollectedSetsMux = &sync.Mutex{}

	// logs to Windows event log
	logger service.Logger

	// contains the running directory of the application
	runningDir string

	// contains the name of the host running the application
	hostName string
)

type program struct {
	exit chan struct{}
}

// Start is called when the service is started
func (p *program) Start(s service.Service) error {
	logger.Info("Starting...")

	// If running under terminal
	if service.Interactive() {
		r, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		runningDir = r
	} else { // else running under service manager
		r, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			log.Fatal(err)
		}
		runningDir = r
	}
	p.exit = make(chan struct{})

	// Start should not block. Do the actual work async.
	go func() {
		if err := p.run(); err != nil {
			logger.Error(err)
		}
	}()

	return nil
}

// Stop is called when the service is stopped
func (p *program) Stop(s service.Service) error {
	// Any work in Stop should be quick, usually a few seconds at most.
	logger.Info("Shutting down...")
	close(p.exit)
	return nil
}

// Contains all code for starting the application
func (p *program) run() error {
	// TODO: find a better way to handle a consistent logs directory across different start methods (service or terminal)
	if *logDirectory == "logs" {
		*logDirectory = filepath.Join(runningDir, *logDirectory)
	}

	// Setup log path to log messages out to
	if l, err := InitLogging(*logDirectory, *logLevel, *JSONOutput); err != nil {
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

	if h, err := os.Hostname(); err == nil {
		hostName = strings.ToLower(h)
	} else {
		return err
	}

	// TODO: find a better way to handle a consistent config path across different start methods (service or terminal)
	if *config == "config.yml" {
		*config = filepath.Join(runningDir, *config)
	}
	go watchFile(*config, configChan, errorsChan)

	go func() {
		for {
			select {
			case <- configChan:
				log.WithField("host", hostName).Infof("%s changed\n", *config)
				go ReadConfigFile(*config)
			case err := <-errorsChan:
				log.Fatalf("error occurred while watching %s -> %s\n", *config, err)
			}

			time.Sleep(5 * time.Second)
		}
	}()

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*addr, nil))

	return nil
}

func main() {
	flag.Parse()

	svcConfig := &service.Config{
		Name:        "pdhexport",
		DisplayName: "pdhexport",
		Description: "A service for exporting windows pdh counters into a Prometheus exporter format available at http://localhost:8080 (or custom specified port).",
	}

	prg := &program{}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}
	errs := make(chan error, 5)
	logger, err = s.Logger(errs)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		for {
			err := <-errs
			if err != nil {
				log.Print(err)
			}
		}
	}()

	// check if a control method was specified for the service
	if len(*svcFlag) != 0 {
		err := service.Control(s, *svcFlag)
		if err != nil {
			log.Printf("Valid actions: %q\n", service.ControlAction)
			log.Fatal(err)
		}
		return
	}
	err = s.Run()
	if err != nil {
		logger.Error(err)
	}
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

// ReadConfigFile will parse the Yaml formatted file and pass along all PdhHostSet that are new to addPCSChan
func ReadConfigFile(file string) {
	log.WithFields(log.Fields{
		"host": hostName,
		"config": file,
	}).Debug("Reading config file")

	newPCSCollectedSets := map[string]*PdhCounter.PdhHostSet{}
	config := NewConfig(file)

	for _, h := range config.HostNames {
		lh := h == "localhost"
		if lh {
			h = hostName
		}

		// if the hostname has not already been processed
		if _, ok := newPCSCollectedSets[h]; !ok {
			log.WithField("host", "h").Debug("Determining counters for host")

			hostSet := &PdhCounter.PdhHostSet{
				Done:        make(chan struct{}),
				Host:        h,
				Interval:    time.Duration(config.Interval) * time.Second,
				IsLocalhost: lh,
			}

			// Build a list of all counters that should be excluded from collection for this host
			exCounters := map[string]struct{}{}
			for k, v := range config.ExcludeCounters {
				if matched, _ := regexp.MatchString(k, h); matched {
					for _, counter := range v {
						exCounters[counter] = struct{}{}
					}
				}
			}

			// Add all counters that should be collected for this host
			for k, v := range config.Counters {
				if matched, _ := regexp.MatchString(k, h); matched {
					counterloop:
					for _, counterPath := range v {
						p, err := PdhCounter.NewPdhCounter(h, counterPath)
						if err != nil {
							log.WithFields(log.Fields{
								"host": h,
								"counter": counterPath,
							}).Errorf("Error experienced in NewPdhCounter -> %s", err)
							continue counterloop
						}

						// if counterPath has an exact match in exCounters then don't add it to hostSet
						if _, ok := exCounters[counterPath]; !ok {
							for exK := range exCounters {
								if exP, err := PdhCounter.NewPdhCounter(h, exK); err != nil {
									panic(err)
								} else if p.ContainsPdhCounter(exP) {
									// exclude the individual instance of exP
									p.ExcludeInstances = append(p.ExcludeInstances, exP.Instance())
								} else if exP.ContainsPdhCounter(p) {
									// exclude p completely
									continue counterloop
								}
							}
							hostSet.Counters = append(hostSet.Counters, p)
						}
					}
				}
			}

			log.WithFields(log.Fields{
				"counterCount": len(hostSet.Counters),
				"host":         hostSet.Host,
			}).Debug("Finished determining counters for host")
			newPCSCollectedSets[h] = hostSet
		}
	}

	// Figure out if any PdhHostSet have been removed completely from collection
	for hostName, cSet := range PCSCollectedSets {
		if _, ok := newPCSCollectedSets[hostName]; !ok {
			// stop the old collection set
			cSet.StopCollect()
		}
	}

	// Figure out if any PdhHostSet have been added or changed
	for hostName, cSet := range newPCSCollectedSets {
		newSet := true

		PCSCollectedSetsMux.Lock()
		v, ok := PCSCollectedSets[hostName]
		PCSCollectedSetsMux.Unlock()
		if ok {
			if !v.TestEquivalence(cSet) {
				newSet = true

				// stop the old collection set
				v.StopCollect()
			} else {
				newSet = false
			}
		}

		if len(cSet.Counters) > 0 && newSet {
			go func(cSet *PdhCounter.PdhHostSet) {
				PCSCollectedSetsMux.Lock()
				PCSCollectedSets[cSet.Host] = cSet
				PCSCollectedSetsMux.Unlock()

				if err := cSet.StartCollect(); err != nil {
					log.WithFields(log.Fields{
						"host": cSet.Host,
					}).Errorf("Error experienced in StartCollect -> %s", err)
				}

				PCSCollectedSetsMux.Lock()
				delete(PCSCollectedSets, cSet.Host)
				PCSCollectedSetsMux.Unlock()

				log.WithFields(log.Fields{
					"host": cSet.Host,
				}).Info("finished StartCollect()\n")
			}(cSet)

			log.Debugf("%s: sent new PdhHostSet\n", hostName)
		}
	}
}