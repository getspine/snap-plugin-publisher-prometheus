package prometheus

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/cenkalti/backoff"

	"github.com/intelsdi-x/snap/control/plugin"
	"github.com/intelsdi-x/snap/control/plugin/cpolicy"
	"github.com/intelsdi-x/snap/core"
	"github.com/intelsdi-x/snap/core/ctypes"
)

const (
	name       = "prometheus"
	version    = 1
	pluginType = plugin.PublisherPluginType
	timeout    = 5
)

var (
	// The maximum time a connection can sit around unused.
	maxConnectionIdle = time.Minute * 5
	// How frequently idle connections are checked
	watchConnectionWait = time.Minute * 1
	// Our connection pool
	clientPool = make(map[string]*clientConnection)
	// Mutex for synchronizing connection pool changes
	m             = &sync.Mutex{}
	invalidMetric = regexp.MustCompile("[^a-zA-Z0-9:_]")
	invalidLabel  = regexp.MustCompile("[^a-zA-Z0-9_]")
)

type clientConnection struct {
	Key      string
	Conn     *http.Client
	LastUsed time.Time
}

func watchConnections() {
	for {
		time.Sleep(watchConnectionWait)
		for k, c := range clientPool {
			if time.Now().Sub(c.LastUsed) > maxConnectionIdle {
				m.Lock()
				delete(clientPool, k)
				m.Unlock()
			}
		}
	}
}

func init() {
	go watchConnections()
}

// Meta returns a plugin meta data
func Meta() *plugin.PluginMeta {
	return plugin.NewPluginMeta(name, version, pluginType, []string{plugin.SnapGOBContentType}, []string{plugin.SnapGOBContentType})
}

//NewPrometheusPublisher returns an instance of the Prometheus publisher
func NewPrometheusPublisher() *prometheusPublisher {
	return &prometheusPublisher{}
}

type prometheusPublisher struct {
}

func (p *prometheusPublisher) GetConfigPolicy() (*cpolicy.ConfigPolicy, error) {
	cp := cpolicy.New()
	config := cpolicy.NewPolicyNode()

	r1, err := cpolicy.NewStringRule("host", true)
	if err != nil {
		panic(err)
	}
	r1.Description = "Prometheus push gateway host"
	config.Add(r1)

	r2, err := cpolicy.NewIntegerRule("port", true)
	if err != nil {
		panic(err)
	}
	r2.Description = "Prometheus push gateway port"
	config.Add(r2)

	r3, err := cpolicy.NewBoolRule("https", true)
	if err != nil {
		panic(err)
	}
	r3.Description = "Prometheus push gateway port"
	config.Add(r3)

	r4, err := cpolicy.NewBoolRule("debug", true)
	if err != nil {
		panic(err)
	}
	r4.Description = "Prometheus debug"
	config.Add(r4)

	r5, err := cpolicy.NewStringRule("job", false, "unused")
	if err != nil {
		panic(err)
	}
	r5.Description = "Prometheus job"
	config.Add(r5)

	r6, err := cpolicy.NewStringRule("instance", false, "")
	if err != nil {
		panic(err)
	}
	r6.Description = "Prometheus instance"
	config.Add(r6)

	r7, err := cpolicy.NewIntegerRule("retries", false, 10)
	if err != nil {
		panic(err)
	}
	r7.Description = "Number of retries"
	config.Add(r7)

	r8, err := cpolicy.NewIntegerRule("timeout_secs", false, 10)
	if err != nil {
		panic(err)
	}
	r8.Description = "Prometheus push timeout seconds"
	config.Add(r8)

	r9, err := cpolicy.NewBoolRule("replace", false, false)
	if err != nil {
		panic(err)
	}
	r9.Description = "Replace metrics within Pushgateway upon push"
	config.Add(r9)

	cp.Add([]string{""}, config)
	return cp, nil
}

// Publish publishes metric data to Prometheus.
func (p *prometheusPublisher) Publish(contentType string, content []byte, config map[string]ctypes.ConfigValue) error {
	logger := log.New()
	var metrics []plugin.MetricType

	switch contentType {
	case plugin.SnapGOBContentType:
		dec := gob.NewDecoder(bytes.NewBuffer(content))
		if err := dec.Decode(&metrics); err != nil {
			logger.Printf("Error decoding GOB: error=%v content=%v", err, content)
			return err
		}
	case plugin.SnapJSONContentType:
		err := json.Unmarshal(content, &metrics)
		if err != nil {
			logger.Printf("Error decoding JSON: error=%v content=%v", err, content)
			return err
		}
	default:
		logger.Printf("Error unknown content type '%v'", contentType)
		return fmt.Errorf("Unknown content type '%s'", contentType)
	}

	promUrl, err := prometheusUrl(config)
	if err != nil {
		panic(err)
	}

	b := backoff.NewExponentialBackOff()
	retries := config["retries"].(ctypes.ConfigValueInt).Value
	for retry := 0; retry < retries; retry++ {
		forceRefresh := retry > 0

		client, err := selectClient(config, forceRefresh)
		if err != nil {
			logger.Printf("Could not select a Prometheus client (retry %d of %d): %v",
				retry, retries, err)
			if retry+1 < retries {
				backoffDuration := b.NextBackOff()
				logger.Printf("Backing off next Prometheus request by: %v", backoffDuration)
				time.Sleep(backoffDuration)
			}
			continue
		}

		err = sendMetrics(config, promUrl, client, metrics)
		if err != nil {
			logger.Printf("Could not send metrics to Prometheus (retry %d of %d): %v",
				retry, retries, err)
			if retry+1 < retries {
				backoffDuration := b.NextBackOff()
				logger.Printf("Backing off next Prometheus request by: %v", backoffDuration)
				time.Sleep(backoffDuration)
			}
			continue
		}

		break
	}

	return nil
}

func sendMetrics(config map[string]ctypes.ConfigValue,
	promUrl *url.URL, client *clientConnection, metrics []plugin.MetricType) error {
	logger := getLogger(config)
	buf := new(bytes.Buffer)
	for _, m := range metrics {
		name, tags, value, _ := mangleMetric(m, config)
		buf.WriteString(prometheusString(name, tags, value))
		buf.WriteByte('\n')
	}

	// A PUT will update the value of a metric for a job, a POST will replace those metrics
	// with those that were pushed.
	httpMethod := "PUT"
	shouldReplace := config["replace"].(ctypes.ConfigValueBool).Value
	if shouldReplace {
		httpMethod = "POST"
	}

	req, err := http.NewRequest(httpMethod, promUrl.String(), bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	res, err := client.Conn.Do(req)
	if err != nil {
		logger.Error("Error sending data to Prometheus: %v", err)
		return err
	}
	defer res.Body.Close()
	_, err = ioutil.ReadAll(res.Body)
	if err != nil {
		logger.Error("Error getting Prometheus response: %v", err)
		return err
	}
	return nil
}

func prometheusString(name string, tags map[string]string, value string) string {
	tmp1 := []string{}
	for k, v := range tags {
		tmp1 = append(tmp1, fmt.Sprintf("%s=\"%s\"", k, v))
	}
	return fmt.Sprintf("%s{%s} %s",
		name,
		strings.Join(tmp1, ","),
		value,
	)
}

func mangleMetric(m plugin.MetricType,
	config map[string]ctypes.ConfigValue) (name string,
	tags map[string]string, value string, ts int64) {
	tags = make(map[string]string)
	ns := m.Namespace().Strings()
	isDynamic, indexes := m.Namespace().IsDynamic()
	if isDynamic {
		for i, j := range indexes {
			// The second return value from IsDynamic(), in this case `indexes`, is the index of
			// the dynamic element in the unmodified namespace. However, here we're deleting
			// elements, which is problematic when the number of dynamic elements in a namespace is
			// greater than 1. Therefore, we subtract i (the loop iteration) from j
			// (the original index) to compensate.
			//
			// Remove "data" from the namespace and create a tag for it
			ns = append(ns[:j-i], ns[j-i+1:]...)
			tags[m.Namespace()[j].Name] = m.Namespace()[j].Value
		}
	}

	for i, v := range ns {
		ns[i] = invalidMetric.ReplaceAllString(v, "_")
	}

	// Add "unit"" if we do not already have a "unit" tag
	if _, ok := m.Tags()["unit"]; !ok {
		tags["unit"] = m.Unit()
	}

	// Process the tags for this metric
	for k, v := range m.Tags() {
		// Convert the standard tag describing where the plugin is running to "source"
		if k == core.STD_TAG_PLUGIN_RUNNING_ON {
			// Unless the "source" tag is already being used
			if _, ok := m.Tags()["source"]; !ok {
				tags["source"] = v
			}
			if _, ok := m.Tags()["host"]; !ok {
				tags["host"] = v
			}
		} else {
			tags[invalidLabel.ReplaceAllString(k, "_")] = v
		}
	}

	name = strings.Join(ns, "_")
	value = fmt.Sprint(m.Data())
	ts = m.Timestamp().Unix() * 1000
	return
}

func prometheusUrl(config map[string]ctypes.ConfigValue) (*url.URL, error) {
	var prefix = "http"
	if config["https"].(ctypes.ConfigValueBool).Value {
		prefix = "https"
	}

	instance := config["instance"].(ctypes.ConfigValueStr).Value

	var u *url.URL
	var err error

	if len(instance) > 0 {
		u, err = url.Parse(fmt.Sprintf("%s://%s:%d/metrics/job/%s/instance/%s",
			prefix,
			config["host"].(ctypes.ConfigValueStr).Value,
			config["port"].(ctypes.ConfigValueInt).Value,
			config["job"].(ctypes.ConfigValueStr).Value,
			instance,
		))
	} else {
		u, err = url.Parse(fmt.Sprintf("%s://%s:%d/metrics/job/%s",
			prefix,
			config["host"].(ctypes.ConfigValueStr).Value,
			config["port"].(ctypes.ConfigValueInt).Value,
			config["job"].(ctypes.ConfigValueStr).Value,
		))
	}

	if err != nil {
		return nil, err
	}
	return u, nil
}

func selectClient(
	config map[string]ctypes.ConfigValue, forceRefresh bool) (*clientConnection, error) {
	// This is not an ideal way to get the logger but deferring solving this for a later date
	logger := getLogger(config)

	// Pool changes need to be safe (read & write) since the plugin can be called concurrently by snapteld.
	m.Lock()
	defer m.Unlock()

	promUrl, err := prometheusUrl(config)
	key := fmt.Sprintf("%s", promUrl.String())

	// Do we have a existing client?
	if clientPool[key] == nil || forceRefresh {
		// create one and add to the pool
		timeoutSecs := int64(config["timeout_secs"].(ctypes.ConfigValueInt).Value)
		con := &http.Client{
			Timeout: time.Second * time.Duration(timeoutSecs),
		}

		if err != nil {
			return nil, err
		}

		cCon := &clientConnection{
			Key:      key,
			Conn:     con,
			LastUsed: time.Now(),
		}
		// Add to the pool
		clientPool[key] = cCon

		logger.Debug("Opening new Prometheus connection[", promUrl.String(), "]")
		return clientPool[key], nil
	}
	// Update when it was accessed
	clientPool[key].LastUsed = time.Now()
	// Return it
	logger.Debug("Using open Prometheus connection[", promUrl.String(), "]")
	return clientPool[key], nil
}

func getLogger(config map[string]ctypes.ConfigValue) *log.Entry {
	logger := log.WithFields(log.Fields{
		"plugin-name":    name,
		"plugin-version": version,
		"plugin-type":    pluginType.String(),
	})

	// default
	log.SetLevel(log.WarnLevel)

	if debug, ok := config["debug"]; ok {
		switch v := debug.(type) {
		case ctypes.ConfigValueBool:
			if v.Value {
				log.SetLevel(log.DebugLevel)
				return logger
			}
		default:
			logger.WithFields(log.Fields{
				"field":         "debug",
				"type":          v,
				"expected type": "ctypes.ConfigValueBool",
			}).Error("invalid config type")
		}
	}

	if loglevel, ok := config["log-level"]; ok {
		switch v := loglevel.(type) {
		case ctypes.ConfigValueStr:
			switch strings.ToLower(v.Value) {
			case "warn":
				log.SetLevel(log.WarnLevel)
			case "error":
				log.SetLevel(log.ErrorLevel)
			case "debug":
				log.SetLevel(log.DebugLevel)
			case "info":
				log.SetLevel(log.InfoLevel)
			default:
				log.WithFields(log.Fields{
					"value":             strings.ToLower(v.Value),
					"acceptable values": "warn, error, debug, info",
				}).Warn("invalid config value")
			}
		default:
			logger.WithFields(log.Fields{
				"field":         "log-level",
				"type":          v,
				"expected type": "ctypes.ConfigValueStr",
			}).Error("invalid config type")
		}
	}

	return logger
}
