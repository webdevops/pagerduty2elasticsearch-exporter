package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/PagerDuty/go-pagerduty"
	elasticsearch "github.com/elastic/go-elasticsearch/v7"
	esapi "github.com/elastic/go-elasticsearch/v7/esapi"
	"github.com/prometheus/client_golang/prometheus"
	"net/http"
	"sync"
	"time"
)

type (
	PagerdutyElasticsearchExporter struct {
		scrapeTime *time.Duration

		elasticSearchClient    *elasticsearch.Client
		elasticsearchIndexName string
		elasticsearchBatchCount int

		pagerdutyClient    *pagerduty.Client
		pagerdutyDateRange *time.Duration

		prometheus struct {
			incident         *prometheus.CounterVec
			incidentLogEntry *prometheus.CounterVec
			duration         *prometheus.GaugeVec
		}
	}

	ElasticsearchIncident struct {
		DocumentID  string `json:"_id,omitempty"`
		Timestamp  string `json:"@timestamp,omitempty"`
		IncidentId string `json:"@incident,omitempty"`
		*pagerduty.Incident
	}

	ElasticsearchIncidentLog struct {
		DocumentID  string `json:"_id,omitempty"`
		Timestamp  string `json:"@timestamp,omitempty"`
		IncidentId string `json:"@incident,omitempty"`
		*pagerduty.LogEntry
	}
)

func (e *PagerdutyElasticsearchExporter) Init() {
	e.elasticsearchBatchCount = 10

	e.prometheus.incident = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pagerduty2es_incident_counter",
			Help: "PagerDuty2es incident counter",
		},
		[]string{},
	)

	e.prometheus.incidentLogEntry = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pagerduty2es_incident_logentry_counter",
			Help: "PagerDuty2es incident logentry counter",
		},
		[]string{},
	)

	e.prometheus.duration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pagerduty2es_duration",
			Help: "PagerDuty2es duration",
		},
		[]string{},
	)

	prometheus.MustRegister(e.prometheus.incident)
	prometheus.MustRegister(e.prometheus.incidentLogEntry)
	prometheus.MustRegister(e.prometheus.duration)
}

func (e *PagerdutyElasticsearchExporter) SetScrapeTime(value time.Duration) {
	e.scrapeTime = &value
}

func (e *PagerdutyElasticsearchExporter) ConnectPagerduty(token string, httpClient *http.Client) {
	e.pagerdutyClient = pagerduty.NewClient(opts.PagerDutyAuthToken)
	e.pagerdutyClient.HTTPClient = httpClient
}

func (e *PagerdutyElasticsearchExporter) SetPagerdutyDateRange(value time.Duration) {
	e.pagerdutyDateRange = &value
}

func (e *PagerdutyElasticsearchExporter) ConnectElasticsearch(cfg elasticsearch.Config, indexName string) {
	var err error
	e.elasticSearchClient, err = elasticsearch.NewClient(cfg)
	if err != nil {
		panic(err)
	}

	_, err = e.elasticSearchClient.Info()
	if err != nil {
		panic(err)
	}

	e.elasticsearchIndexName = indexName
}

func (e *PagerdutyElasticsearchExporter) Run() {
	go func() {
		for {
			e.runScrape()
			e.sleepUntilNextCollection()
		}
	}()
}

func (e *PagerdutyElasticsearchExporter) sleepUntilNextCollection() {
	daemonLogger.Verbosef("sleeping %v", e.scrapeTime)
	time.Sleep(*e.scrapeTime)
}

func (e *PagerdutyElasticsearchExporter) runScrape() {
	var wgProcess sync.WaitGroup
	daemonLogger.Verbosef("Starting scraping")

	since := time.Now().Add(-*e.pagerdutyDateRange).Format(time.RFC3339)
	listOpts := pagerduty.ListIncidentsOptions{
		Since: since,
	}
	listOpts.Limit = PagerdutyIncidentLimit
	listOpts.Offset = 0

	esIndexRequestChannel := make(chan *esapi.IndexRequest, e.elasticsearchBatchCount)

	startTime := time.Now()

	// index from channel
	wgProcess.Add(1)
	go func() {
		defer wgProcess.Done()
		for esIndexRequest := range esIndexRequestChannel {
			e.doESIndexRequest(esIndexRequest)
		}
	}()

	for {
		incidentResponse, err := e.pagerdutyClient.ListIncidents(listOpts)
		if err != nil {
			panic(err)
		}

		for _, incident := range incidentResponse.Incidents {
			daemonLogger.Verbosef(" - Incident %v", incident.Id)
			e.indexIncident(incident, esIndexRequestChannel)

			listLogOpts := pagerduty.ListIncidentLogEntriesOptions{}
			incidentLogResponse, err := e.pagerdutyClient.ListIncidentLogEntries(incident.Id, listLogOpts)
			if err != nil {
				panic(err)
			}

			for _, logEntry := range incidentLogResponse.LogEntries {
				daemonLogger.Verbosef("   - LogEntry %v", logEntry.ID)
				e.indexIncidentLogEntry(incident, logEntry, esIndexRequestChannel)
			}
		}

		if !incidentResponse.More {
			break
		}
		listOpts.Offset += listOpts.Limit
	}
	close(esIndexRequestChannel)

	wgProcess.Wait()

	duration := time.Now().Sub(startTime)
	e.prometheus.duration.WithLabelValues().Set(duration.Seconds())
	daemonLogger.Verbosef("processing took %v", duration.String())
}

func (e *PagerdutyElasticsearchExporter) indexIncident(incident pagerduty.Incident, callback chan<- *esapi.IndexRequest) {
	e.prometheus.incident.WithLabelValues().Inc()

	esIncident := ElasticsearchIncident{
		Timestamp:  incident.CreatedAt,
		IncidentId: incident.Id,
		Incident:   &incident,
	}
	incidentJson, _ := json.Marshal(esIncident)

	req := esapi.IndexRequest{
		Index:      opts.ElasticsearchIndex,
		DocumentID: fmt.Sprintf("incident-%v", incident.Id),
		Body:       bytes.NewReader(incidentJson),
	}
	callback <- &req
}

func (e *PagerdutyElasticsearchExporter) indexIncidentLogEntry(incident pagerduty.Incident, logEntry pagerduty.LogEntry, callback chan<- *esapi.IndexRequest) {
	e.prometheus.incidentLogEntry.WithLabelValues().Inc()

	esLogEntry := ElasticsearchIncidentLog{
		Timestamp:  logEntry.CreatedAt,
		IncidentId: incident.Id,
		LogEntry:   &logEntry,
	}
	logEntryJson, _ := json.Marshal(esLogEntry)

	req := esapi.IndexRequest{
		Index:      opts.ElasticsearchIndex,
		DocumentID: fmt.Sprintf("logentry-%v", logEntry.ID),
		Body:       bytes.NewReader(logEntryJson),
	}
	callback <- &req
}

func (e *PagerdutyElasticsearchExporter) doESIndexRequest(req *esapi.IndexRequest) {
	res, err := req.Do(context.Background(), e.elasticSearchClient)
	if err != nil {
		fmt.Println(err)
	}
	defer res.Body.Close()
}
