// +build windows
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
	"github.com/sirupsen/logrus"
)

var (
	addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
	config = flag.String("config", "config.yml", "Fully qualified path to yml formatted config file.")
	logDirectory = flag.String("logDirectory", "logs", "Specify a directory where logs should be written to. Use \"\" to logger to stdout.")
	logLevel = flag.String("logLevel", "info", "Use this flag to specify what level of logging you wish to have output. Available values: panic, fatal, error, warn, info, debug.")
	JSONOutput = flag.Bool("JSONOutput", false, "Use this flag to turn on json formatted logging.")
	svcFlag = flag.String("service", "", "Control the system service. Valid actions: start, stop, restart, install, uninstall")

	// A map containing a reference to all pdhQuery that are being collected
	PdhQueries = PdhCounter.NewPdhQueryMap(nil)

	// contains the running directory of the application
	runningDir string

	// contains the name of the host running the application
	hostName string
	logger   *logrus.Logger
)

type program struct {
	exit chan struct{}
}

// Start is called when the service is started
func (p *program) Start(s service.Service) error {
	logger.Info("starting...")

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
	logger.Info("stopping...")
	close(p.exit)
	return nil
}

// Contains all code for starting the application
func (p *program) run() error {
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
		g.Add(
			func() error {
				PdhQueries = PdhCounter.NewPdhQueryMap(logger)
				return PdhQueries.Listen()
			},
			func(err error) {
				close(PdhQueries.CancelChan)
			},
		)
	}
	{
		// Config file watcher
		g.Add(
			func() error {
				if *config == "config.yml" {
					*config = filepath.Join(runningDir, *config)
				}

				return watchFile(*config, ReadConfigFile, cancelChan)
			},
			func(err error) {
				close(cancelChan)
			},
		)
	}
	//{
	//	// test error creator
	//	g.Add(
	//		func() error {
	//			time.Sleep(10 * time.Second)
	//
	//			return fmt.Errorf("test error")
	//		},
	//		func(err error) {
	//			// do nothing
	//		},
	//	)
	//}
	if err := g.Run(); err != nil {
		logger.Fatal(err)
	}

	return nil
}

func main() {
	flag.Parse()

	// determine the runningDir (absolute path) of pdhexport.exe whether it is ran from CLI or service manager
	if service.Interactive() {
		// running in CLI
		r, err := os.Getwd()
		if err != nil {
			logger.Fatal(err)
		}
		runningDir = r
	} else {
		// running under service manager
		r, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			logger.Fatal(err)
		}
		runningDir = r
	}

	// setup logging to file
	l, close, err := InitLogging(*logDirectory, *logLevel, *JSONOutput)
	if err != nil {
		logger.Fatalf("error initializing logger file -> %v\n", err)
	}
	defer func() {
		test := 2
		fmt.Println("closing log",test)
		close()
	}()
	logger = l

	// create the windows service
	s, err := service.New(&program{exit:make(chan struct{})}, &service.Config{
		Name:        "pdhexport",
		DisplayName: "pdhexport",
		Description: fmt.Sprintf("A service for exporting windows pdh counters into a Prometheus exporter available at %s.", *addr),
	})
	if err != nil {
		logger.Fatal(err)
	}

	// check if a control method was specified for the windows service
	if len(*svcFlag) != 0 {
		logger.WithField("svcFlag", *svcFlag).Debug("received service control flag")
		err := service.Control(s, *svcFlag)
		if err != nil {
			logger.Fatalf("%s. Valid actions: %q\n", err, service.ControlAction)
		}
		logger.Debug("returning from main()")
		return
	}

	// The following will be executed when pdhexport is ran from CLI.
	logger.Debug("calling s.Run() in main()")
	err = s.Run()
	if err != nil {
		logger.Fatal(err)
	}
	logger.Info("goodbye!")
}

// InitLogging is used to initialize all properties of the logrus
// logging library.
func InitLogging(logDirectory string, logLevel string, jsonOutput bool) (logger *logrus.Logger, close func(), err error) {
	logger = logrus.New()
	var file *os.File

	// if LogDirectory is "" then logging will just go to stdout
	if logDirectory != "" {
		// if the default "logs" was specified then we need to turn it into an absolute path for service manager
		if logDirectory == "logs" {
			logDirectory = filepath.Join(runningDir, logDirectory)
		}

		if _, err = os.Stat(logDirectory); os.IsNotExist(err) {
			err := os.MkdirAll(logDirectory, 0777)
			if err != nil {
				return nil, nil, err
			}

			// Chmod is needed because the permissions can't be set by the Mkdir function in Linux
			err = os.Chmod(logDirectory, 0777)
			if err != nil {
				return nil, nil, err
			}
		}
		file, err = os.OpenFile(filepath.Join(logDirectory, fmt.Sprintf("%s%s", time.Now().Local().Format("20060102"), ".log")), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, nil, err
		}
		//logger.SetOutput(file)
		logger.Out = file
	} else {
		// Output to stdout instead of the default stderr
		//logrus.SetOutput(os.Stdout)
		logger.Out = os.Stdout
	}

	if jsonOutput {
		//logger.SetFormatter(&logrus.JSONFormatter{})
		logger.Formatter = &logrus.JSONFormatter{}
	} else {
		//logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
		logger.Formatter = &logrus.TextFormatter{FullTimestamp: true}
	}

	l, err := logrus.ParseLevel(strings.ToLower(logLevel))
	if err != nil {
		logger.SetLevel(logrus.InfoLevel)
	} else {
		logger.SetLevel(l)
	}

	close = func() {
		if err = file.Close(); err != nil {
			logger.Errorf("error closing logger file -> %v\n", err)
		}
	}

	return logger, close, nil
}

// watchFile watches the file located at filePath for changes and sends a message through
// the channel fileChangedChan when the file has been changed. If an error occurs, it will be
// sent through the channel errorsChan.
func watchFile(file string, fileChangeHandler func(filePath string) error, cancelChan chan struct{}) error {
	var initialStat os.FileInfo
	loop:
	for {
		stat, err := os.Stat(file)
		if err != nil {
			return err
		}

		if initialStat == nil || stat.Size() != initialStat.Size() || stat.ModTime() != initialStat.ModTime() {
			initialStat = stat
			err := fileChangeHandler(file)
			if err != nil {
				return err
			}
		}

		select{
		case <- cancelChan:
			break loop // must specify name of loop or else it will just break out of select{}
		case <- time.After(5 * time.Second):
		}
	}

	return nil
}

// ReadConfigFile will parse the Yaml formatted file and pass along all pdhHostSet that are new to addPCSChan
func ReadConfigFile(file string) error {
	logger.WithFields(logrus.Fields{
		"host": hostName,
		"config": file,
	}).Debug("Reading config file")

	newPdhQueries := PdhCounter.NewPdhQueryMap(logger)
	config := NewConfig(file, logger)

	for _, h := range config.HostNames {
		lh := h == "localhost"
		if lh {
			h = hostName
		}

		// if the hostname has not already been processed
		if newPdhQueries.GetQuery(h) == nil {
			logger.WithField("host", "h").Debug("Determining counters for host")

			i := time.Duration(config.Interval) * time.Second
			query := PdhCounter.NewPdhQuery(h, i, lh, logger)

			// Build a list of all counters that should be excluded from collection for this host
			exCounters := map[PdhCounter.PdhPath]struct{}{}
			for k, v := range config.ExcludeCounters {
				matched, err := regexp.MatchString(k, h)
				if err != nil {
					return err
				}
				if matched {
					for _, counter := range v {
						exCounters[counter] = struct{}{}
					}
				}
			}

			// Add all counters that should be collected for this host
			for k, v := range config.Counters {
				matched, err := regexp.MatchString(k, h)
				if err != nil {
					return err
				}
				if matched {
					counterloop:
					for _, counterPath := range v {
						p, err := PdhCounter.NewPdhCounter(h, counterPath, logger)
						if err != nil {
							return err
							// TODO: is returning err here good? or should the err be handled without returning?
							//logger.WithFields(logrus.Fields{
							//	"host": h,
							//	"counter": counterPath,
							//}).Errorf("Error experienced in NewPdhCounter -> %s", err)
							//continue counterloop
						}

						// if counterPath has an exact match in exCounters then don't add it to query
						if _, ok := exCounters[counterPath]; !ok {
							for exK := range exCounters {
								if exP, err := PdhCounter.NewPdhCounter(h, exK, logger); err != nil {
									// TODO: is returning err here good? or should the err be handled without returning?
									return err
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

			logger.WithFields(logrus.Fields{
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
		logger.WithFields(logrus.Fields{
			"host": result.Host,
		}).Info("sending new query\n")
		PdhQueries.NewQueryChan <- result
	}

	return nil
}