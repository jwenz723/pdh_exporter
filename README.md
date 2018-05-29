# pdhexport
A Prometheus exporter for Windows Performance Counters using winpdh

This is a work in progress and is not expected to be stable or complete.

Define PDH counters and hosts to be collected in the `config.yml` file (reference `config_example.yml` for help with formatting).

Run the application using `go run pdhexport.go` then open a browser and go to http://localhost:8080/metrics

## Installation
1. Build the executable for your system: `Go build`
2. Place the executable where you want it to run from
3. Create a configuration file named config.yml within the same directory as the executable (use config_example.yml as an example configuration file)
4. As an administrator, install the application as a Windows Service: `pdhexport.exe -service install`
5. Start the Windows Service: `pdhexport.exe -service start` (you can also control the service using the Windows Services application)
6. Stop the Windows Service: `pdhexport.exe -service stop`

For help run: `pdhexport.exe -h`