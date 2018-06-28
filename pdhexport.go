package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jwenz723/pdhexport/PdhCounter"
	"github.com/kardianos/service"
	"github.com/oklog/run"
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

	// A map containing a reference to all pdhQuery that are being collected
	PdhQueries = PdhCounter.NewPdhQueryMap()

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
	log.Info("Starting...")

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
			log.Error(err)
		}
	}()

	return nil
}

// Stop is called when the service is stopped
func (p *program) Stop(s service.Service) error {
	// Any work in Stop should be quick, usually a few seconds at most.
	log.Info("Shutting down...")
	close(p.exit)
	return nil
}

// Contains all code for starting the application
func (p *program) run() error {
	configChan := make(chan struct{})
	cancelChan := make(chan struct{})

	if h, err := os.Hostname(); err == nil {
		hostName = strings.ToLower(h)
	} else {
		return err
	}

	var g run.Group
	{
		// Expose the registered prometheus metrics via HTTP.
		ln, _ := net.Listen("tcp", *addr)
		g.Add(
			func() error {
				http.Handle("/metrics", promhttp.Handler())
				return http.Serve(ln, nil)
			},
			func(err error) {
				ln.Close()
			},
		)
	}
	{
		// Config file watcher
		g.Add(
			func() error {
				// TODO: find a better way to handle a consistent config path across different start methods (service or terminal)
				if *config == "config.yml" {
					*config = filepath.Join(runningDir, *config)
				}
				return watchFile(*config, configChan, cancelChan)
			},
			func(err error) {
				close(cancelChan)
			},
		)
	}
	{
		g.Add(
			func() error {
				for {
					select {
					case <- configChan:
						log.WithField("host", hostName).Infof("%s changed\n", *config)
						go ReadConfigFile(*config)
					case result := <- PdhQueries.DeadQueryChan:
						log.WithField("host", result.Host).Info("query stopped")
						log.Infof("queries remaining: %d\n", PdhQueries.Length())

						// TODO: figure out a way to stop collection when 0 queries remain. This currently will stop when a query is replaced if only 1 query exists
						//if PdhQueries.Length() == 0 {
						//	return fmt.Errorf("no more queries to collect")
						//}
					case err := <- PdhQueries.ErrorsChan:
						log.WithFields(log.Fields{
							"host": err.Host,
							"error": err.Err,
						}).Error("query error")
					}
				}
			},
			func(err error) {
				close(configChan)
				close(PdhQueries.DeadQueryChan)
				close(PdhQueries.ErrorsChan)
			},
		)
	}
	if err := g.Run(); err != nil {
		log.Fatal(err)
	}

	return nil
}

func main() {
	flag.Parse()

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
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		for {
			err := <-errs
			if err != nil {
				log.Error(err)
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
		log.Fatal(err)
	}
	log.Info("goodbye!")
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
func watchFile(filePath string, fileChangedChan, cancelChan chan struct{}) error {
	var initialStat os.FileInfo
	loop:
	for {
		stat, err := os.Stat(filePath)
		if err != nil {
			return err
		}

		if initialStat == nil || stat.Size() != initialStat.Size() || stat.ModTime() != initialStat.ModTime() {
			initialStat = stat
			fileChangedChan <- struct{}{}
		}

		select{
		case <- cancelChan:
			break loop // must specify name of loop or else it will just break out of select{}
		case <- time.After(1 * time.Second):
			// do nothing
		}
	}

	return nil
}

// ReadConfigFile will parse the Yaml formatted file and pass along all pdhHostSet that are new to addPCSChan
func ReadConfigFile(file string) {
	log.WithFields(log.Fields{
		"host": hostName,
		"config": file,
	}).Debug("Reading config file")

	newPdhQueries := PdhCounter.NewPdhQueryMap()
	config := NewConfig(file)

	for _, h := range config.HostNames {
		lh := h == "localhost"
		if lh {
			h = hostName
		}

		// if the hostname has not already been processed
		if newPdhQueries.GetQuery(h) == nil {
			log.WithField("host", "h").Debug("Determining counters for host")

			i := time.Duration(config.Interval) * time.Second
			query, err := PdhCounter.NewPdhQuery(h, i, lh)
			if err != nil {
				log.WithFields(log.Fields{
					"host": h,
					"interval": i,
					"isLocalHost": lh,
				}).Fatalf("error creating new pdhQuery -> %s", err)
			}

			// Build a list of all counters that should be excluded from collection for this host
			exCounters := map[PdhCounter.PdhPath]struct{}{}
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

						// if counterPath has an exact match in exCounters then don't add it to query
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
							query.AddCounter(p)
						}
					}
				}
			}

			log.WithFields(log.Fields{
				"counterCount": query.NumCounters(),
				"host":         query.Host,
			}).Debug("Finished determining counters for host")
			newPdhQueries.Add(h, query)
		}
	}

	// Figure out if any hosts have been removed completely from collection
	for qResult := range PdhQueries.IterateMap() {
		if newPdhQueries.GetQuery(hostName) == nil {
			// stop the old query
			qResult.Query.Stop()
		}
	}

	// send all queries to be collected
	for result := range newPdhQueries.IterateMap() {
		log.WithFields(log.Fields{
			"host": result.Host,
		}).Info("sending new query\n")
		PdhQueries.NewQueryChan <- result
	}
}