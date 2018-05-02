# pdhexport
A Prometheus exporter for Windows Performance Counters using winpdh

This is a work in progress and is not expected to be stable or complete.

Define PDH counters and hosts to be collected in the `config.yml` file (reference `config_example.yml` for help with formatting).

Run the application using `go run pdhexport.go` then open a browser and go to http://localhost:8080/metrics
