package main

import (
	"io/ioutil"
	"strings"

	"github.com/jwenz723/pdh_exporter/PdhCounter"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// Config defines a struct to match a configuration yaml file.
type Config struct {
	Counters        	map[string][]PdhCounter.PdhPath    	`yaml:"Counters"`
	ExcludeCounters		map[string][]PdhCounter.PdhPath 	`yaml:"ExcludeCounters"`
	HostNames       	[]string                            `yaml:"HostNames"`
	Interval        	int64                              	`yaml:"Interval"`
}

// NewConfig will create a new Config instance from the specified yaml file
func NewConfig(yamlFile string, log *logrus.Logger) (config Config) {
	source, readFileErr := ioutil.ReadFile(yamlFile)
	if readFileErr != nil {
		log.Fatal(readFileErr)
	}

	readYamlErr := yaml.Unmarshal(source, &config)
	if readYamlErr != nil {
		log.Fatal(readYamlErr)
	}

	// Force all keys in config.Counters to be lowercase
	t := map[string][]PdhCounter.PdhPath{}
	for k, v := range config.Counters {
		t[strings.ToLower(k)] = v
	}
	config.Counters = t

	// Force all keys in config.ExcludeCounters to be lowercase
	e := map[string][]PdhCounter.PdhPath{}
	for k, v := range config.ExcludeCounters {
		e[strings.ToLower(k)] = v
	}
	config.ExcludeCounters = e

	// Force all values in config.HostNames to be lowercase
	for i, h := range config.HostNames {
		config.HostNames[i] = strings.ToLower(h)
	}

	return
}