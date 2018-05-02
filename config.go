package main

import (
	"io/ioutil"
	"log"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Pdh_Counters PdhCounters
}

type PdhCounters struct {
	Counters      map[string][]PdhCounter
	HostNames     []string
	Interval      int64
	MetricPrefix string
}

func NewConfig(config_path string) (config Config) {

	source, readfile_err := ioutil.ReadFile(config_path)
	if readfile_err != nil {
		log.Fatal(readfile_err)
	}

	readyaml_err := yaml.Unmarshal(source, &config)
	if readyaml_err != nil {
		log.Fatal(readyaml_err)
	}

	return
}
