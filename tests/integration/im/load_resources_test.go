package im_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type loadResource struct {
	AtMS        float64 `json:"at_ms"`
	ScrapeMS    float64 `json:"scrape_ms"`
	CPUSeconds  float64 `json:"cpu_seconds"`
	RSSBytes    float64 `json:"rss_bytes"`
	Goroutines  float64 `json:"goroutines"`
	Connections float64 `json:"websocket_connections"`
}

type loadResourceReport struct {
	TotalScrapeMS float64        `json:"total_scrape_ms"`
	Samples       []loadResource `json:"samples"`
	CPUSeconds    float64        `json:"cpu_seconds_delta"`
	PeakRSSBytes  float64        `json:"peak_sampled_rss_bytes"`
}

func loadResources(ctx context.Context, base string, start time.Time) (loadResource, error) {
	scrapeStart := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", base+"/metrics", nil)
	if err != nil {
		return loadResource{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return loadResource{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return loadResource{}, fmt.Errorf("metrics status=%d", response.StatusCode)
	}
	values := map[string]float64{}
	wanted := map[string]bool{"process_cpu_seconds_total": true, "process_resident_memory_bytes": true, "go_goroutines": true, "gostream_websocket_connections": true}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || !wanted[fields[0]] {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return loadResource{}, err
		}
		values[fields[0]] = v
	}
	if err := scanner.Err(); err != nil {
		return loadResource{}, err
	}
	if len(values) != len(wanted) {
		return loadResource{}, fmt.Errorf("missing required API process metrics")
	}
	return loadResource{AtMS: float64(time.Since(start).Microseconds()) / 1000, ScrapeMS: float64(time.Since(scrapeStart).Microseconds()) / 1000, CPUSeconds: values["process_cpu_seconds_total"], RSSBytes: values["process_resident_memory_bytes"], Goroutines: values["go_goroutines"], Connections: values["gostream_websocket_connections"]}, nil
}

func summarizeResources(samples []loadResource) loadResourceReport {
	report := loadResourceReport{Samples: samples}
	if len(samples) > 1 {
		report.CPUSeconds = samples[len(samples)-1].CPUSeconds - samples[0].CPUSeconds
	}
	for _, s := range samples {
		report.TotalScrapeMS += s.ScrapeMS
		report.PeakRSSBytes = max(report.PeakRSSBytes, s.RSSBytes)
	}
	return report
}
