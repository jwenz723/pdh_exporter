package main

import (
	"io/ioutil"
	"log"
	"strings"
	"sync"

	"gopkg.in/yaml.v2"
)

// Config defines a struct to match a configuration yaml file.
type Config struct {
	Counters        	map[string][]string `yaml:"Counters"`
	CountersLock		sync.RWMutex
	CountersSequence 	int
	ExcludeCounters		map[string][]string `yaml:"ExcludeCounters"`
	HostNames       	[]string `yaml:"HostNames"`
	Interval        	int64 `yaml:"Interval"`
}

// UnmarshalYAML overrides what happens when the yaml.Unmarshal function is executed on the Config type
func (c *Config) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type RawConfig Config
	if err := unmarshal((*RawConfig)(c)); err != nil {
		return err
	}

	// Force all keys in c.Counters to be lowercase
	t := map[string][]string{}
	for k, v := range c.Counters {
		t[strings.ToLower(k)] = v
	}
	c.Counters = t

	// Force all keys in c.ExcludeCounters to be lowercase
	e := map[string][]string{}
	for k, v := range c.ExcludeCounters {
		e[strings.ToLower(k)] = v
	}
	c.ExcludeCounters = e

	// Force all values in c.HostNames to be lowercase
	for i, h := range c.HostNames {
		c.HostNames[i] = strings.ToLower(h)
	}
	return nil
}

// NewConfig will create a new Config instance from the specified yaml file
func NewConfig(yamlFile string) (config Config) {
	source, readFileErr := ioutil.ReadFile(yamlFile)
	if readFileErr != nil {
		log.Fatal(readFileErr)
	}

	readYamlErr := yaml.Unmarshal(source, &config)
	if readYamlErr != nil {
		log.Fatal(readYamlErr)
	}

	return
}