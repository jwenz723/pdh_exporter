# pdhexport
A Prometheus exporter for Windows Performance Counters using winpdh

This is a work in progress and is not expected to be stable or complete.

Define counters to be collected in the counters.txt file.
Run the application using `go run pdhexport.go` then open a browser and go to http://localhost:8080
