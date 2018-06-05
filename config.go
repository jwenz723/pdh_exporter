package main

import (
	"io/ioutil"
	"log"
	"strings"

	"gopkg.in/yaml.v2"
)

// Config defines a struct to match a configuration yaml file.
type Config struct {
	Counters        map[string][]string `yaml:"Counters"`
	ExcludeCounters map[string][]string `yaml:"ExcludeCounters"`
	HostNames       []string `yaml:"HostNames"`
	Interval        int64 `yaml:"Interval"`
}

// UnmarshalYAML overrides what happens when the yaml.Unmarshal function is executed on the Config type
func (c *Config) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type RawConfig Config
	if err := unmarshal((*RawConfig)(c)); err != nil {
		return err
	}

	// Force all keys in c.Counters to be uppercase
	t := map[string][]string{}
	for k, v := range c.Counters {
		t[strings.ToUpper(k)] = v
	}
	c.Counters = t

	// Force all keys in c.ExcludeCounters to be uppercase
	e := map[string][]string{}
	for k, v := range c.ExcludeCounters {
		e[strings.ToUpper(k)] = v
	}
	c.ExcludeCounters = e

	// Force all values in c.HostNames to be uppercase
	for i, h := range c.HostNames {
		c.HostNames[i] = strings.ToUpper(h)
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