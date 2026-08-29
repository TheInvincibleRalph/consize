// Package cloudwatch implements dbmetrics.Source against Amazon RDS via
// CloudWatch, mirroring the k8s Prometheus path: ListInstances pages RDS
// DescribeDBInstances (query protocol: form-encoded POST, XML response);
// Series folds GetMetricStatistics averages (AWS JSON protocol) into
// step-aligned buckets with the store's db_* metric names. Requests are
// signed with SigV4 via the shared pricing.AWSSigner — no AWS SDK, the
// repo stays dependency-light (same rationale as pricing).
//
// Instance mapping: dbmetrics.Instance has no Region, Engine, MultiAZ or
// Storage fields, so those are carried as Labels on the instance
// (consize.savings.dev/engine, /region, /multi-az, /storage). RDS TagList
// entries become labels too — an operator opts a live instance into
// automatic class changes the same way the fixture does, by tagging it
// consize.savings.dev/auto-db=enabled. The derived facts above are set
// after tags so they cannot be clobbered.
//
// Metric mapping (bucket metric ← CloudWatch):
//
//	db_cpu_percent   ← CPUUtilization Average (already a percent)
//	db_mem_percent   ← 100 × (1 − FreeableMemory_bytes / (class GiB × 2^30)),
//	                   class memory from the analysis catalog
//	db_iops          ← ReadIOPS + WriteIOPS Average
//	db_connections   ← DatabaseConnections Average
//	db_errors        ← NOT AVAILABLE: CloudWatch publishes no error
//	                   counter for RDS instances. Series returns no data,
//	                   and the verifier treats a fully-missing metric as
//	                   no-evidence (SLI "unavailable", never FAIL).
//
// Configuration: CONSIZE_AWS_REGION (default us-east-1), CONSIZE_DB_FILTER
// (comma-separated instance identifiers; empty = all), and the standard
// AWS credentials env vars (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY /
// AWS_SESSION_TOKEN) — the same convention pricing.NewAWS uses.
package cloudwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"consize/internal/analysis"
	"consize/internal/config"
	"consize/internal/dbmetrics"
	"consize/internal/pricing"
	"consize/internal/store"
)

// maxChunk is the GetMetricStatistics time-range cap (86,400s); the API
// also caps a single call at 1,440 data points, so Series chunks windows
// into 24h slices (96 points at the 15m step).
const maxChunk = 24 * time.Hour

// Source is a dbmetrics.Source backed by RDS + CloudWatch.
type Source struct {
	region string
	filter []string // DB instance identifiers; empty = all

	accessKey, secretKey, sessionToken string
	now                                func() time.Time // signature clock, tests

	rdsBase string // https://rds.<region>.amazonaws.com (tests override)
	cwBase  string // https://monitoring.<region>.amazonaws.com
	client  *http.Client
}

// NewSource builds a live RDS/CloudWatch source from the environment
// (CONSIZE_AWS_REGION, CONSIZE_DB_FILTER, AWS credentials).
func NewSource() *Source {
	region := config.Str("CONSIZE_AWS_REGION", "us-east-1")
	var filter []string
	for _, id := range strings.Split(config.Str("CONSIZE_DB_FILTER", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			filter = append(filter, id)
		}
	}
	return &Source{
		region:       region,
		filter:       filter,
		accessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		secretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
		sessionToken: os.Getenv("AWS_SESSION_TOKEN"),
		rdsBase:      "https://rds." + region + ".amazonaws.com",
		cwBase:       "https://monitoring." + region + ".amazonaws.com",
		client:       &http.Client{Timeout: 60 * time.Second},
	}
}

// signer builds the SigV4 signer for one AWS service (rds | monitoring).
func (s *Source) signer(service string) *pricing.AWSSigner {
	return &pricing.AWSSigner{
		AccessKey: s.accessKey, SecretKey: s.secretKey, SessionToken: s.sessionToken,
		Region: s.region, Service: service, Now: s.now,
	}
}

// --- RDS DescribeDBInstances (query protocol: form POST, XML response) ---

// rdsInstance is the slice of DescribeDBInstances we consume.
type rdsInstance struct {
	Identifier  string   `xml:"DBInstanceIdentifier"`
	Class       string   `xml:"DBInstanceClass"`
	Engine      string   `xml:"Engine"`
	MultiAZ     bool     `xml:"MultiAZ"`
	Maintenance string   `xml:"PreferredMaintenanceWindow"` // UTC ddd:hh:mm-ddd:hh:mm
	AllocatedGB int      `xml:"AllocatedStorage"`
	Status      string   `xml:"DBInstanceStatus"`
	Tags        []rdsTag `xml:"TagList>Tag"`
}

type rdsTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// describeDBInstancesResp unwraps the query-protocol response
// DescribeDBInstancesResponse > DescribeDBInstancesResult.
type describeDBInstancesResp struct {
	Result struct {
		DBInstances []rdsInstance `xml:"DBInstances>DBInstance"`
		Marker      string        `xml:"Marker"`
	} `xml:"DescribeDBInstancesResult"`
}

// ListInstances pages DescribeDBInstances and maps each DB instance to a
// dbmetrics.Instance. CONSIZE_DB_FILTER restricts the result client-side
// (pagination stays a single marker loop).
func (s *Source) ListInstances(ctx context.Context) ([]dbmetrics.Instance, error) {
	var out []dbmetrics.Instance
	marker := ""
	for {
		form := url.Values{
			"Action":  {"DescribeDBInstances"},
			"Version": {"2014-10-31"},
		}
		if marker != "" {
			form.Set("Marker", marker)
		}
		body, err := s.signedPost(ctx, s.rdsBase, "rds", "/", []byte(form.Encode()),
			"application/x-www-form-urlencoded", "")
		if err != nil {
			return nil, err
		}
		var d describeDBInstancesResp
		if err := xml.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("decode DescribeDBInstances response: %w", err)
		}
		for _, i := range d.Result.DBInstances {
			if !s.wanted(i.Identifier) {
				continue
			}
			out = append(out, mapInstance(i, s.region))
		}
		if d.Result.Marker == "" {
			return out, nil
		}
		marker = d.Result.Marker
	}
}

// wanted reports whether an identifier passes the CONSIZE_DB_FILTER
// (empty filter = all instances).
func (s *Source) wanted(id string) bool {
	if len(s.filter) == 0 {
		return true
	}
	for _, f := range s.filter {
		if f == id {
			return true
		}
	}
	return false
}

// mapInstance maps one RDS row. dbmetrics.Instance carries no Region,
// Engine, MultiAZ or Storage fields, so those ride as labels (see the
// package doc). MultiAZ instances report 2 replicas (primary + standby).
func mapInstance(i rdsInstance, region string) dbmetrics.Instance {
	replicas := 1
	if i.MultiAZ {
		replicas = 2
	}
	labels := map[string]string{}
	for _, t := range i.Tags {
		labels[t.Key] = t.Value
	}
	// Derived facts last: tags cannot clobber them.
	labels["consize.savings.dev/engine"] = i.Engine
	labels["consize.savings.dev/region"] = region
	labels["consize.savings.dev/multi-az"] = strconv.FormatBool(i.MultiAZ)
	labels["consize.savings.dev/storage"] = strconv.Itoa(i.AllocatedGB) + "GB"

	return dbmetrics.Instance{
		Name:              i.Identifier,
		Namespace:         "rds",
		Class:             i.Class,
		Replicas:          replicas,
		MaintenanceWindow: i.Maintenance,
		Provider:          "aws",
		Labels:            labels,
	}
}

// --- CloudWatch GetMetricStatistics (AWS JSON protocol) ---

// cwDatapoint is one GetMetricStatistics Average.
type cwDatapoint struct {
	TS      time.Time
	Average float64
	Samples int
}

// getMetricStatisticsResp is the JSON response shape.
type getMetricStatisticsResp struct {
	Datapoints []struct {
		Timestamp   string  `json:"Timestamp"`
		Average     float64 `json:"Average"`
		SampleCount float64 `json:"SampleCount"`
	} `json:"Datapoints"`
}

// Series folds GetMetricStatistics averages into step-aligned buckets over
// [start, end), one bucket per window the source observed (CloudWatch
// omits empty periods, like Prometheus omits missing samples). Buckets are
// single-sample windows: P50=P95=P99=Max=the period Average.
func (s *Source) Series(ctx context.Context, inst dbmetrics.Instance, metric string,
	start, end time.Time, step time.Duration) ([]store.Bucket, error) {

	if metric == store.MetricDBErrors {
		// CloudWatch publishes no error counter for RDS instances; the
		// verifier treats a fully-missing metric as no-evidence ("unavailable"
		// SLI, never FAIL), so no data is the correct contract.
		return nil, nil
	}
	var cwNames []string
	switch metric {
	case store.MetricDBCPUPercent:
		cwNames = []string{"CPUUtilization"}
	case store.MetricDBMemPercent:
		cwNames = []string{"FreeableMemory"}
	case store.MetricDBIOPS:
		cwNames = []string{"ReadIOPS", "WriteIOPS"}
	case store.MetricDBConnections:
		cwNames = []string{"DatabaseConnections"}
	default:
		return nil, nil // unknown metric: no data, like a source that doesn't emit it
	}

	start = start.UTC().Truncate(step)
	end = end.UTC()
	if !end.After(start) {
		return nil, nil
	}
	var class *analysis.DBClass
	if metric == store.MetricDBMemPercent {
		// Mem percent needs the class memory baseline. An instance whose
		// class is not in the catalog yields no mem evidence (the analyzer
		// would skip it anyway).
		if c, ok := analysis.DBClassByName(inst.Class); ok {
			class = &c
		} else {
			return nil, nil
		}
	}

	// One series per CloudWatch metric, merged per aligned window start.
	perName := make([][]cwDatapoint, len(cwNames))
	for i, n := range cwNames {
		pts, err := s.fetchSeries(ctx, inst.Name, n, start, end, step)
		if err != nil {
			return nil, err
		}
		perName[i] = pts
	}
	merged := map[int64]*mergedPoint{}
	for i, pts := range perName {
		for _, p := range pts {
			ts := p.TS.UTC().Truncate(step).Unix()
			if ts < start.Unix() || ts >= end.Unix() {
				continue
			}
			mp := merged[ts]
			if mp == nil {
				mp = &mergedPoint{values: make([]float64, len(cwNames))}
				merged[ts] = mp
			}
			mp.values[i] = p.Average
			if p.Samples > mp.samples {
				mp.samples = p.Samples
			}
		}
	}

	keys := make([]int64, 0, len(merged))
	for ts := range merged {
		keys = append(keys, ts)
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })

	out := make([]store.Bucket, 0, len(keys))
	for _, ts := range keys {
		mp := merged[ts]
		var v float64
		switch metric {
		case store.MetricDBMemPercent:
			free := mp.values[0]
			v = 100 * (1 - free/(class.MemGiB*float64(1<<30)))
			if v < 0 {
				v = 0 // FreeableMemory can dip negative on small instances
			} else if v > 100 {
				v = 100
			}
		case store.MetricDBIOPS:
			v = mp.values[0] + mp.values[1]
		default:
			v = mp.values[0]
		}
		samples := mp.samples
		if samples < 1 {
			samples = 1
		}
		out = append(out, store.Bucket{
			WorkloadID:  -1, // assigned by the collector
			Metric:      metric,
			WindowStart: time.Unix(ts, 0).UTC(),
			P50:         v, P95: v, P99: v, Max: v,
			Samples: samples,
		})
	}
	return out, nil
}

// mergedPoint is one aligned window's contributions.
type mergedPoint struct {
	values  []float64
	samples int
}

// fetchSeries pulls one CloudWatch metric over [start, end) in ≤24h
// chunks (the API's time-range cap), preserving order.
func (s *Source) fetchSeries(ctx context.Context, identifier, cwMetric string,
	start, end time.Time, step time.Duration) ([]cwDatapoint, error) {

	var out []cwDatapoint
	for t := start; t.Before(end); {
		ce := t.Add(maxChunk)
		if ce.After(end) {
			ce = end
		}
		pts, err := s.getMetricStatistics(ctx, identifier, cwMetric, t, ce, step)
		if err != nil {
			return nil, err
		}
		out = append(out, pts...)
		t = ce
	}
	return out, nil
}

// getMetricStatistics issues one signed GetMetricStatistics call.
func (s *Source) getMetricStatistics(ctx context.Context, identifier, cwMetric string,
	start, end time.Time, step time.Duration) ([]cwDatapoint, error) {

	reqBody, err := json.Marshal(map[string]any{
		"Namespace":  "AWS/RDS",
		"MetricName": cwMetric,
		"Dimensions": []map[string]string{{"Name": "DBInstanceIdentifier", "Value": identifier}},
		"StartTime":  start.UTC().Format(time.RFC3339),
		"EndTime":    end.UTC().Format(time.RFC3339),
		"Period":     int(step.Seconds()),
		"Statistics": []string{"Average"},
	})
	if err != nil {
		return nil, err
	}
	resp, err := s.signedPost(ctx, s.cwBase, "monitoring", "/", reqBody,
		"application/x-amz-json-1.1", "AmazonCloudWatch.GetMetricStatistics")
	if err != nil {
		return nil, err
	}
	var d getMetricStatisticsResp
	if err := json.Unmarshal(resp, &d); err != nil {
		return nil, fmt.Errorf("decode GetMetricStatistics response: %w", err)
	}
	out := make([]cwDatapoint, 0, len(d.Datapoints))
	for _, p := range d.Datapoints {
		ts, err := time.Parse(time.RFC3339, p.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("GetMetricStatistics timestamp %q: %w", p.Timestamp, err)
		}
		out = append(out, cwDatapoint{
			TS: ts, Average: p.Average, Samples: int(p.SampleCount),
		})
	}
	return out, nil
}

// signedPost signs and sends one AWS POST and returns the body on 200.
// target may be empty (query-protocol requests carry no X-Amz-Target).
func (s *Source) signedPost(ctx context.Context, baseURL, service, path string,
	body []byte, contentType, target string) ([]byte, error) {

	extra := map[string]string{"content-type": contentType}
	if target != "" {
		extra["x-amz-target"] = target
	}
	hdr := s.signer(service).Sign(http.MethodPost, hostOf(baseURL), path, string(body), extra)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s: status %s: %s", service, resp.Status, b)
	}
	return io.ReadAll(resp.Body)
}

// hostOf strips the scheme (and path) from a base URL.
func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}
