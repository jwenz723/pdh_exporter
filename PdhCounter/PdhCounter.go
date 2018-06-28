package PdhCounter

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unsafe"

	"github.com/jwenz723/win"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// PdhPath is a string representing a PDH path.
// Examples:
// - \Processor(*)\% Processor Time
// - \\MachineName\Memory\Available Bytes
type PdhPath string

func (p PdhPath) String() string {
	return string(p)
}

// AppendHostname will append a hostname to the beginning of PdhPath
func (p *PdhPath) AppendHostname (hostname string) {
	*p = PdhPath(fmt.Sprintf(`\\%s%s`, hostname, p.String()))
}

// pdhCounter is a single PDH counter (with a possible * instance) and all registered
// prometheus collectors (1 per PDH instance) that report values for the single PDH counter
type pdhCounter struct{
	Path             		PdhPath `yaml:"PdhPath"` // The path to the PDH counter to be collected
	ExcludeInstances 		[]string              // A list of PDH instances to be excluded from collection
	MachineName      		string
	ObjectName       		string
	InstanceName     		string
	ParentInstance   		string
	InstanceIndex    		uint32
	CounterName      		string
	handle 			 		*win.PDH_HCOUNTER
	collectionFailures 		int
	promCollectors	 		map[string]*prometheus.Gauge
	promCollectorsLock 		*sync.RWMutex
}

// Instance returns the combination of ParentInstance, InstanceName, and InstanceIndex according to
// https://msdn.microsoft.com/en-us/library/windows/desktop/aa373193%28v=vs.85%29.aspx?f=255&MSPPError=-2147217396
func (p *pdhCounter) Instance() string {
	pi := ""
	ii := ""

	if p.ParentInstance != "" {
		pi = fmt.Sprintf("%s/", p.ParentInstance)
	}

	if p.InstanceIndex > 0 {
		ii = fmt.Sprintf("#%d", p.InstanceIndex)
	}

	return fmt.Sprintf("%s%s%s", pi, p.InstanceName, ii)
}

// ContainsPdhCounter will test if p contains c
func (p *pdhCounter) ContainsPdhCounter(c *pdhCounter) bool {
	if p.Path == c.Path {
		return true
	} else if p.MachineName == c.MachineName && p.ObjectName == c.ObjectName && p.CounterName == c.CounterName {
		if p.Instance() == "*" {
			return true
		} else if len(p.InstanceName) > 0 && p.InstanceName[:len(p.InstanceName)-1] == c.InstanceName { // if p.InstanceName is something like chrome*
			return true
		}
	}
	return false
}

// parsePdhCounterPathElements will parse elements from c into p
func (p *pdhCounter) parsePdhCounterPathElements(c win.PDH_COUNTER_PATH_ELEMENTS) {
	if c.MachineName != nil {
		p.MachineName = win.UTF16PtrToString(c.MachineName)[2:]
	}

	if c.ObjectName != nil {
		p.ObjectName = win.UTF16PtrToString(c.ObjectName)
	}

	if c.InstanceName != nil {
		p.InstanceName = win.UTF16PtrToString(c.InstanceName)
	}

	if c.ParentInstance != nil {
		p.ParentInstance = win.UTF16PtrToString(c.ParentInstance)
	}

	p.InstanceIndex = c.InstanceIndex

	if c.CounterName != nil {
		p.CounterName = win.UTF16PtrToString(c.CounterName)
	}
}

// registerPromCollector will register the pdhCounter instance with prometheus
func (c *pdhCounter) registerPromCollector(instance string, g *prometheus.GaugeOpts) error {
	c.promCollectorsLock.Lock()
	defer c.promCollectorsLock.Unlock()
	t := prometheus.NewGauge(*g)
	if err := prometheus.Register(t); err != nil {
		return err
	} else {
		c.promCollectors[instance] = &t
		return nil
	}
}

// unregisterPromCollectors will unregister from prometheus all prometheus collectors that exist for this pdhCounter
func (c *pdhCounter) unregisterPromCollectors() {
	for k, v := range c.promCollectors {
		if b := prometheus.Unregister(*v); !b {
			log.WithFields(log.Fields{
				"collector": k,
			}).Error("failed to unregister Prometheus Collector\n")
		} else {
			delete(c.promCollectors, k)
			log.WithFields(log.Fields{
				"collector": k,
			}).Debug("unregistered Prometheus Collector")
		}
	}
}

// counterToPrometheusGauge converts a windows performance counter string into
// a prometheus Gauge.
//
// According to https://prometheus.io/docs/concepts/data_model/
// 		- Prometheus Metric Names must match: [a-zA-Z_:][a-zA-Z0-9_:]*
//		- Prometheus Label Restrictions:
// 			- Label names must match: [a-zA-Z_][a-zA-Z0-9_]*
//			- Label values: may contain any Unicode characters
//
// Additional Prometheus Metric/Label naming conventions: https://prometheus.io/docs/practices/naming/
func (c *pdhCounter) counterToPrometheusGauge(instance string) (*prometheus.GaugeOpts, error) {
	// Replace known runes that occur in winpdh that aren't recommended in prometheus
	r := strings.NewReplacer(
		".", "_",
		"-", "_",
		" ", "_",
		"/","_",
		"%", "percent",
	)
	counterName := r.Replace(c.CounterName)
	instance = r.Replace(instance)

	// Use this regex to replace any invalid characters that weren't accounted for already
	reg, err := regexp.Compile("[^a-zA-Z0-9_:]")
	if err != nil {
		return nil, err
	}

	category := string(reg.ReplaceAll([]byte(c.ObjectName),[]byte("")))
	instance = string(reg.ReplaceAll([]byte(instance),[]byte("")))

	l := prometheus.Labels{"hostname": c.MachineName, "pdhcategory": category}
	if instance != "" {
		l["pdhinstance"] = instance
	}

	return &prometheus.GaugeOpts{
		ConstLabels: l,
		Help:        "windows performance counter",
		Name:        string(reg.ReplaceAll([]byte(counterName), []byte(""))),
		Namespace:   "winpdh",
	}, nil
}

// NewPdhCounter will create a new pdhCounter instance with all fields populated if the specified path is valid
func NewPdhCounter(hostname string, path PdhPath) (*pdhCounter, error) {
	if hostname != "" {
		path.AppendHostname(hostname)// fmt.Sprintf(`\\%s%s`, hostname, path.String())
	}

	var b uint32
	if ret := win.PdhParseCounterPath(path.String(), nil, &b); ret == win.PDH_MORE_DATA {
		buf := make([]byte, b)
		if ret := win.PdhParseCounterPath(path.String(), &buf[0], &b); ret == win.ERROR_SUCCESS {
			c := *(*win.PDH_COUNTER_PATH_ELEMENTS)(unsafe.Pointer(&buf[0]))
			p := pdhCounter{
				ExcludeInstances: []string{},
				Path: path,
				promCollectors: map[string]*prometheus.Gauge{},
				promCollectorsLock: &sync.RWMutex{},
			}
			p.parsePdhCounterPathElements(c)

			return &p, nil
		} else {
			// Failed to parse counter
			// Possible error codes: PDH_INVALID_ARGUMENT, PDH_INVALID_PATH, PDH_MEMORY_ALLOCATION_FAILURE
			return nil, errors.New("failed to create pdhCounter from " + path.String() + " -> " + fmt.Sprintf("%x", ret))
		}
	} else {
		// Failed to obtain buffer info
		// Possible error codes: PDH_INVALID_ARGUMENT, PDH_INVALID_PATH, PDH_MEMORY_ALLOCATION_FAILURE
		return nil, errors.New("failed to obtain buffer info for pdhCounter from " + path.String() + " -> " + fmt.Sprintf("%x", ret))
	}
}

