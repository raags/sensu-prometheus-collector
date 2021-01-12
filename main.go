package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/prometheus/client_golang/api/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/smira/go-statsd"
)

const (
	exporterAuthID = "exporter"
)

type ExporterAuth struct {
	User     string `envconfig:"user" default:""`
	Password string `envconfig:"password" default:""`
	Header   string `envconfig:"header" default:""`
}

type Tag struct {
	Name  model.LabelName
	Value model.LabelValue
}

type Metric struct {
	Tags  []Tag
	Value float64
}

func CreateJSONMetrics(samples model.Vector) string {
	metrics := []Metric{}

	for _, sample := range samples {
		metric := Metric{}

		for name, value := range sample.Metric {
			tag := Tag{
				Name:  name,
				Value: value,
			}

			metric.Tags = append(metric.Tags, tag)
		}

		metric.Value = float64(sample.Value)

		metrics = append(metrics, metric)
	}

	jsonMetrics, _ := json.Marshal(metrics)

	return string(jsonMetrics)
}

func SendToStatsD(samples model.Vector, metricPrefix string, globalTagsArr []string, host string, port string) {
	s := statsd.NewClient(host+":"+port, statsd.TagStyle(statsd.TagFormatDatadog), statsd.MetricPrefix(metricPrefix))
	defer s.Close()

	var globalTags []statsd.Tag
	if len(globalTagsArr) > 0 {
		for _, tagString := range globalTagsArr {
			tagkv := strings.Split(tagString, ":")
			tag := statsd.StringTag(strings.TrimSpace(tagkv[0]), strings.TrimSpace(tagkv[1]))
			globalTags = append(globalTags, tag)
		}
	}

	for _, sample := range samples {
		name := string(sample.Metric["__name__"])

		var metricTags []statsd.Tag
		for name, value := range sample.Metric {
			if name != "__name__" {
				tag := statsd.StringTag(string(name), string(value))
				metricTags = append(metricTags, tag)
			}
		}

		tags := append(globalTags, metricTags...)
		s.Gauge(name, int64(sample.Value), tags...)
	}
}

func CreateGraphiteMetrics(samples model.Vector, metricPrefix string) string {
	metrics := ""

	for _, sample := range samples {
		name := fmt.Sprintf("%s%s", metricPrefix, sample.Metric["__name__"])

		value := strconv.FormatFloat(float64(sample.Value), 'f', -1, 64)

		now := time.Now()
		timestamp := now.Unix()

		metric := fmt.Sprintf("%s %s %d\n", name, value, timestamp)

		metrics += metric
	}

	return metrics
}

func CreateInfluxMetrics(samples model.Vector, metricPrefix string) string {
	metrics := ""

	for _, sample := range samples {
		metric := fmt.Sprintf("%s%s", metricPrefix, sample.Metric["__name__"])

		for name, value := range sample.Metric {
			if name != "__name__" {
				tags := fmt.Sprintf(",%s=%s", name, value)
				if !strings.Contains(tags, "\n") && strings.Count(tags, "=") == 1 {
					metric += tags
				}
			}
		}

		metric = strings.Replace(metric, "\n", "", -1)

		value := strconv.FormatFloat(float64(sample.Value), 'f', -1, 64)

		now := time.Now()
		timestamp := now.Unix()

		metric += fmt.Sprintf(" value=%s %d\n", value, timestamp)

		segments := strings.Split(metric, " ")
		if len(segments) == 3 {
			metrics += metric
		}
	}

	return metrics
}

func FilterSamples(samples model.Vector, includeRegex string, excludeRegex string) (model.Vector, error) {
	var reInclude, reExclude *regexp.Regexp
	var err error

	if includeRegex != "" {
		reInclude, err = regexp.Compile(includeRegex)
		if err != nil {
			return nil, err
		}
	}

	if excludeRegex != "" {
		reExclude, err = regexp.Compile(excludeRegex)
		if err != nil {
			return nil, err
		}
	}

	var filteredSamples model.Vector
	for _, sample := range samples {
		metricString := sample.Metric.String()

		var matchInclude, matchExclude bool
		if reInclude == nil {
			// include all metrics
			matchInclude = true
		} else {
			matchInclude = reInclude.MatchString(metricString)
		}

		if reExclude != nil {
			matchExclude = reExclude.MatchString(metricString)
		}

		if matchInclude == true && matchExclude == false {
			filteredSamples = append(filteredSamples, sample)
		}
	}
	return filteredSamples, nil
}

func OutputMetrics(samples model.Vector, outputFormat string, metricPrefix string, globalTagsArr []string, statsdHost string, statsdPort string) error {
	output := ""

	switch outputFormat {
	case "influx":
		output = CreateInfluxMetrics(samples, metricPrefix)
	case "graphite":
		output = CreateGraphiteMetrics(samples, metricPrefix)
	case "json":
		output = CreateJSONMetrics(samples)
	case "sendtostatsd":
		SendToStatsD(samples, metricPrefix, globalTagsArr, statsdHost, statsdPort)
	default:
		log.Println("Error: Unknown output format")
		os.Exit(2)
	}

	fmt.Print(output)

	return nil
}

func QueryPrometheus(promURL string, queryString string) (model.Vector, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	promConfig := prometheus.Config{Address: promURL}
	promClient, err := prometheus.New(promConfig)

	if err != nil {
		return nil, err
	}

	promQueryClient := prometheus.NewQueryAPI(promClient)

	promResponse, err := promQueryClient.Query(ctx, queryString, time.Now())

	if err != nil {
		return nil, err
	}

	if promResponse.Type() == model.ValVector {
		return promResponse.(model.Vector), nil
	}

	return nil, errors.New("unexpected response type")
}

func QueryExporter(exporterURL string, auth ExporterAuth, insecureSkipVerify bool) (model.Vector, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify},
	}
	client := &http.Client{Transport: tr}
	req, err := http.NewRequest("GET", exporterURL, nil)

	if err != nil {
		return nil, err
	}

	if auth.User != "" && auth.Password != "" {
		req.SetBasicAuth(auth.User, auth.Password)
	}

	if auth.Header != "" {
		req.Header.Set("Authorization", auth.Header)
	}

	expResponse, err := client.Do(req)

	if err != nil {
		return nil, err
	}
	defer expResponse.Body.Close()

	if expResponse.StatusCode != http.StatusOK {
		return nil, errors.New("exporter returned non OK HTTP response status: " + expResponse.Status)
	}

	var parser expfmt.TextParser

	metricFamilies, err := parser.TextToMetricFamilies(expResponse.Body)

	if err != nil {
		return nil, err
	}

	samples := model.Vector{}

	decodeOptions := &expfmt.DecodeOptions{
		Timestamp: model.Time(time.Now().Unix()),
	}

	for _, family := range metricFamilies {
		familySamples, _ := expfmt.ExtractSamples(decodeOptions, family)
		samples = append(samples, familySamples...)
	}

	return samples, nil
}

func setExporterAuth(user string, password string, header string) (auth ExporterAuth, error error) {
	err := envconfig.Process(exporterAuthID, &auth)

	if err != nil {
		return auth, err
	}

	if user != "" && password != "" {
		auth.User = user
		auth.Password = password
	}

	if header != "" {
		auth.Header = header
	}

	return auth, nil
}

func main() {
	exporterURL := flag.String("exporter-url", "", "Prometheus exporter URL to pull metrics from.")
	exporterUser := flag.String("exporter-user", "", "Prometheus exporter basic auth user.")
	exporterPassword := flag.String("exporter-password", "", "Prometheus exporter basic auth password.")
	exporterAuthorizationHeader := flag.String("exporter-authorization", "", "Prometheus exporter Authorization header.")
	promURL := flag.String("prom-url", "http://localhost:9090", "Prometheus API URL.")
	queryString := flag.String("prom-query", "up", "Prometheus API query string.")
	outputFormat := flag.String("output-format", "influx", "The check output format to use for metrics {influx|graphite|json|sendtostatsd}.")
	includeRegex := flag.String("include-regex", "", "Regex to include metrics applied agasint the metric in Prometheus exposition format")
	excludeRegex := flag.String("exclude-regex", "", "Regex to exclude metrics, applied after -include-regex")
	statsdHost := flag.String("statsd-host", "localhost", "Statsd hostname for sendtostatsd")
	statsdPort := flag.String("statsd-port", "8125", "Statsd port for sendtostatsd")
	metricPrefix := flag.String("metric-prefix", "", "Metric name prefix, only supported by line protocol output formats.")
	globalTags := flag.String("global-tags", "", "Tags to add to all metrics, colon separated csv e.g. foo:bar,baz:bar")
	insecureSkipVerify := flag.Bool("insecure-skip-verify", false, "Skip TLS peer verification.")
	flag.Parse()

	var samples model.Vector
	var err error

	if *exporterURL != "" {
		auth, err := setExporterAuth(*exporterUser, *exporterPassword, *exporterAuthorizationHeader)

		if err != nil {
			log.Fatal(err)
			os.Exit(2)
		}

		samples, err = QueryExporter(*exporterURL, auth, *insecureSkipVerify)

		if err != nil {
			log.Fatal(err)
			os.Exit(2)
		}

	} else {
		samples, err = QueryPrometheus(*promURL, *queryString)

		if err != nil {
			log.Fatal(err)
			os.Exit(2)
		}
	}

	if *includeRegex != "" || *excludeRegex != "" {
		samples, err = FilterSamples(samples, *includeRegex, *excludeRegex)
		if err != nil {
			log.Println(err)
			os.Exit(2)
		}
	}

	var globalTagsArr []string
	if *globalTags != "" {
		globalTagsTrimed := strings.TrimSpace(*globalTags)
		globalTagsArr = strings.Split(globalTagsTrimed, ",")
	}

	err = OutputMetrics(samples, *outputFormat, *metricPrefix, globalTagsArr, *statsdHost, *statsdPort)

	if err != nil {
		_ = fmt.Errorf("error %v", err)
		os.Exit(2)
	}
}
