// Package collector ingests cluster telemetry into the store: usage
// buckets from Prometheus, workload metadata from the k8s API.
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Series is one Prometheus time series: a label set plus value points.
type Series struct {
	Metric map[string]string
	Points []Point
}

// Point is one timestamped value.
type Point struct {
	Timestamp time.Time
	Value     float64
}

// PrometheusClient is the query_range interface the collector needs.
type PrometheusClient interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error)
}

// HTTPPrometheus is a PrometheusClient against the standard HTTP API.
type HTTPPrometheus struct {
	baseURL string
	client  *http.Client
}

// NewHTTPPrometheus returns a client for the given Prometheus base URL.
func NewHTTPPrometheus(baseURL string, client *http.Client) *HTTPPrometheus {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &HTTPPrometheus{baseURL: stringsTrimSuffixSlash(baseURL), client: client}
}

func stringsTrimSuffixSlash(s string) string {
	if len(s) > 0 && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}

// QueryRange implements PrometheusClient.
func (p *HTTPPrometheus) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	u, err := url.Parse(p.baseURL + "/api/v1/query_range")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", fmt.Sprintf("%ds", int(step.Seconds())))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query_range %q: %w", query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("query_range %q: status %s: %s", query, resp.Status, body)
	}

	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode query_range %q: %w", query, err)
	}
	if envelope.Status != "success" {
		return nil, fmt.Errorf("query_range %q: prometheus status %q", query, envelope.Status)
	}
	var raw []struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	}
	if err := json.Unmarshal(envelope.Data.Result, &raw); err != nil {
		return nil, fmt.Errorf("parse result for %q: %w", query, err)
	}

	out := make([]Series, 0, len(raw))
	for _, r := range raw {
		s := Series{Metric: r.Metric}
		for _, v := range r.Values {
			ts, ok1 := v[0].(float64)
			val, ok2 := strconv.ParseFloat(fmt.Sprintf("%v", v[1]), 64)
			if !ok1 || ok2 != nil {
				continue
			}
			s.Points = append(s.Points, Point{
				Timestamp: time.Unix(int64(ts), 0).UTC(),
				Value:     val,
			})
		}
		out = append(out, s)
	}
	return out, nil
}
