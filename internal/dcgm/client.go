// Package dcgm provides a client for scraping GPU health metrics
// from the DCGM exporter and surfacing per-node summaries.
package dcgm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// NodeHealth summarizes GPU health for a single node.
// One per node, regardless of how many GPUs that node has.
type NodeHealth struct {
	Hostname  string
	MaxTemp   float64 // hottest GPU on the node, in Celsius
	XIDErrors int     // sum of XID errors across all GPUs on the node
}

// Client scrapes the DCGM exporter and returns per-node summaries.
type Client struct {
	URL        string
	HTTPClient *http.Client
}

// NewClient creates a DCGM client pointing at the given metrics URL.
func NewClient(url string) *Client {
	return &Client{
		URL: url,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Scrape fetches and parses metrics, returning a map keyed by hostname.
func (c *Client) Scrape(ctx context.Context) (map[string]*NodeHealth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing metrics: %w", err)
	}

	results := map[string]*NodeHealth{}

	// Helper closure: get-or-create a NodeHealth for a hostname.
	getNode := func(hostname string) *NodeHealth {
		if existing, ok := results[hostname]; ok {
			return existing
		}
		nh := &NodeHealth{Hostname: hostname}
		results[hostname] = nh
		return nh
	}

	// Walk through temperature metrics, track the hottest per node.
	if family, ok := families["DCGM_FI_DEV_GPU_TEMP"]; ok {
		for _, metric := range family.GetMetric() {
			hostname := labelValue(metric.Label, "Hostname")
			if hostname == "" {
				continue
			}
			temp := metric.GetGauge().GetValue()
			nh := getNode(hostname)
			if temp > nh.MaxTemp {
				nh.MaxTemp = temp
			}
		}
	}

	// Walk through XID error metrics, sum per node.
	if family, ok := families["DCGM_FI_DEV_XID_ERRORS"]; ok {
		for _, metric := range family.GetMetric() {
			hostname := labelValue(metric.Label, "Hostname")
			if hostname == "" {
				continue
			}
			errors := int(metric.GetGauge().GetValue())
			nh := getNode(hostname)
			nh.XIDErrors += errors
		}
	}

	return results, nil
}

// labelValue extracts a single label's value from a metric's label list.
func labelValue(labels []*dto.LabelPair, name string) string {
	for _, l := range labels {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}
