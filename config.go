package main

import (
	"io/ioutil"
	"log"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Counters      map[string][]string
	HostNames     []string
	Interval      int64
}

func NewConfig(configPath string) (config Config) {

	source, readFileErr := ioutil.ReadFile(configPath)
	if readFileErr != nil {
		log.Fatal(readFileErr)
	}

	readYamlErr := yaml.Unmarshal(source, &config)
	if readYamlErr != nil {
		log.Fatal(readYamlErr)
	}

	return
}
