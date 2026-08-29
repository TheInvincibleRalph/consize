package cloudwatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"consize/internal/dbmetrics"
	"consize/internal/store"
)

// cannedRDS is one DescribeDBInstances page.
const rdsPage1 = `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
<DescribeDBInstancesResult><DBInstances>
<DBInstance>
  <DBInstanceIdentifier>payments-prod</DBInstanceIdentifier>
  <DBInstanceClass>db.t3.large</DBInstanceClass>
  <Engine>postgres</Engine>
  <MultiAZ>false</MultiAZ>
  <PreferredMaintenanceWindow>sun:00:00-sat:00:00</PreferredMaintenanceWindow>
  <AllocatedStorage>100</AllocatedStorage>
  <DBInstanceStatus>available</DBInstanceStatus>
  <TagList><Tag><Key>consize.savings.dev/auto-db</Key><Value>enabled</Value></Tag></TagList>
</DBInstance>
<DBInstance>
  <DBInstanceIdentifier>analytics</DBInstanceIdentifier>
  <DBInstanceClass>db.t3.medium</DBInstanceClass>
  <Engine>postgres</Engine>
  <MultiAZ>true</MultiAZ>
  <PreferredMaintenanceWindow>mon:02:00-mon:03:00</PreferredMaintenanceWindow>
  <AllocatedStorage>250</AllocatedStorage>
  <DBInstanceStatus>available</DBInstanceStatus>
</DBInstance>
</DBInstances><Marker>page-2</Marker></DescribeDBInstancesResult>
</DescribeDBInstancesResponse>`

const rdsPage2 = `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
<DescribeDBInstancesResult><DBInstances>
<DBInstance>
  <DBInstanceIdentifier>reporting</DBInstanceIdentifier>
  <DBInstanceClass>db.t3.small</DBInstanceClass>
  <Engine>mysql</Engine>
  <MultiAZ>false</MultiAZ>
  <PreferredMaintenanceWindow>wed:04:00-wed:05:00</PreferredMaintenanceWindow>
  <AllocatedStorage>50</AllocatedStorage>
  <DBInstanceStatus>available</DBInstanceStatus>
</DBInstance>
</DBInstances></DescribeDBInstancesResult>
</DescribeDBInstancesResponse>`

// rdsServer serves paginated DescribeDBInstances and records the
// markers it saw plus whether requests were signed.
func rdsServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var markers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("rds body: %v", err)
		}
		if form.Get("Action") != "DescribeDBInstances" || form.Get("Version") != "2014-10-31" {
			t.Fatalf("unexpected form: %v", form)
		}
		if r.Header.Get("Authorization") == "" || r.Header.Get("X-Amz-Date") == "" {
			t.Fatalf("request not signed: %+v", r.Header)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Fatalf("content type: %q", ct)
		}
		markers = append(markers, form.Get("Marker"))
		switch form.Get("Marker") {
		case "":
			w.Write([]byte(rdsPage1))
		case "page-2":
			w.Write([]byte(rdsPage2))
		default:
			t.Fatalf("unexpected marker %q", form.Get("Marker"))
		}
	}))
	return srv, &markers
}

func TestListInstancesPaginationAndMapping(t *testing.T) {
	srv, markers := rdsServer(t)
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: srv.URL, cwBase: "http://unused", client: srv.Client()}

	insts, err := s.ListInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 3 {
		t.Fatalf("want 3 instances across 2 pages, got %d", len(insts))
	}
	if got := *markers; len(got) != 2 || got[0] != "" || got[1] != "page-2" {
		t.Fatalf("pagination markers: %v", got)
	}

	p := insts[0]
	if p.Name != "payments-prod" || p.Class != "db.t3.large" || p.Namespace != "rds" ||
		p.Provider != "aws" || p.Replicas != 1 {
		t.Fatalf("instance 0: %+v", p)
	}
	if p.MaintenanceWindow != "sun:00:00-sat:00:00" {
		t.Fatalf("maintenance window: %q", p.MaintenanceWindow)
	}
	if p.Labels["consize.savings.dev/auto-db"] != "enabled" {
		t.Fatalf("RDS tags must become labels: %v", p.Labels)
	}
	if p.Labels["consize.savings.dev/engine"] != "postgres" ||
		p.Labels["consize.savings.dev/region"] != "us-east-1" ||
		p.Labels["consize.savings.dev/multi-az"] != "false" ||
		p.Labels["consize.savings.dev/storage"] != "100GB" {
		t.Fatalf("derived labels: %v", p.Labels)
	}

	a := insts[1]
	if a.Name != "analytics" || a.Replicas != 2 || a.Labels["consize.savings.dev/multi-az"] != "true" {
		t.Fatalf("MultiAZ instance must report 2 replicas: %+v", a)
	}
	if insts[2].Name != "reporting" || insts[2].Labels["consize.savings.dev/engine"] != "mysql" {
		t.Fatalf("page 2 instance: %+v", insts[2])
	}
}

func TestListInstancesFilter(t *testing.T) {
	srv, _ := rdsServer(t)
	defer srv.Close()
	s := &Source{region: "us-east-1", filter: []string{"payments-prod", "reporting"},
		rdsBase: srv.URL, cwBase: "http://unused", client: srv.Client()}

	insts, err := s.ListInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 || insts[0].Name != "payments-prod" || insts[1].Name != "reporting" {
		t.Fatalf("filter: %+v", insts)
	}
}

// cwServer serves GetMetricStatistics from a canned map and records the
// requests it saw.
func cwServer(t *testing.T, canned map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != "AmazonCloudWatch.GetMetricStatistics" {
			t.Fatalf("x-amz-target: %q", r.Header.Get("X-Amz-Target"))
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-amz-json-1.1" {
			t.Fatalf("content type: %q", ct)
		}
		if r.Header.Get("Authorization") == "" {
			t.Fatal("request not signed")
		}
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		var req struct {
			MetricName string `json:"MetricName"`
		}
		_ = json.Unmarshal(body, &req)
		resp, ok := canned[req.MetricName]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"__type":"UnknownMetricName","message":"no such metric"}`))
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.Write([]byte(resp))
	}))
	return srv, &bodies
}

// onePoint renders a single-datapoint GetMetricStatistics response.
func onePoint(ts string, avg float64, samples float64) string {
	b, _ := json.Marshal(map[string]any{"Label": "x", "Datapoints": []map[string]any{
		{"Timestamp": ts, "Average": avg, "SampleCount": samples},
	}})
	return string(b)
}

func TestSeriesStepAlignedAndWindowExclusive(t *testing.T) {
	// Aligned datapoints at 00:00/00:15/00:30/00:45/01:00; the window is
	// [00:00, 01:00) so the 01:00 point must be dropped.
	canned := map[string]string{
		"CPUUtilization": `{"Label":"CPUUtilization","Datapoints":[
			{"Timestamp":"2026-08-10T00:00:00Z","Average":10.0,"SampleCount":15.0},
			{"Timestamp":"2026-08-10T00:15:00Z","Average":11.0,"SampleCount":15.0},
			{"Timestamp":"2026-08-10T00:30:00Z","Average":12.0,"SampleCount":15.0},
			{"Timestamp":"2026-08-10T00:45:00Z","Average":13.0,"SampleCount":15.0},
			{"Timestamp":"2026-08-10T01:00:00Z","Average":99.0,"SampleCount":15.0}]}`,
	}
	srv, _ := cwServer(t, canned)
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "payments-prod", Class: "db.t3.large"}
	start := time.Date(2026, 8, 10, 0, 3, 0, 0, time.UTC) // unaligned start
	end := time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)   // exclusive end
	bs, err := s.Series(context.Background(), inst, store.MetricDBCPUPercent, start, end, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 4 {
		t.Fatalf("want 4 buckets in [00:00,01:00), got %d", len(bs))
	}
	for i, b := range bs {
		if b.WindowStart.Unix()%900 != 0 {
			t.Fatalf("bucket %d not step-aligned: %s", i, b.WindowStart)
		}
		if b.P50 != b.P95 || b.P95 != b.P99 || b.P99 != b.Max {
			t.Fatalf("single-sample window expected: %+v", b)
		}
		if b.Samples != 15 {
			t.Fatalf("sample count: %d", b.Samples)
		}
	}
	if want := []float64{10, 11, 12, 13}; bs[0].P95 != want[0] || bs[3].P95 != want[3] {
		t.Fatalf("values: %v", bs)
	}
}

func TestSeriesOffGridTimestampTruncatesToStep(t *testing.T) {
	// A datapoint returned at 00:07:30 (period aligned to its own grid)
	// must land in the 00:00 window.
	srv, _ := cwServer(t, map[string]string{
		"DatabaseConnections": onePoint("2026-08-10T00:07:30Z", 300, 12),
	})
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "payments-prod", Class: "db.t3.large"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBConnections, start, start.Add(15*time.Minute), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].WindowStart != start || bs[0].P95 != 300 {
		t.Fatalf("off-grid point: %+v", bs)
	}
}

func TestSeriesIOPSIsSum(t *testing.T) {
	srv, _ := cwServer(t, map[string]string{
		"ReadIOPS":  onePoint("2026-08-10T00:00:00Z", 120, 15),
		"WriteIOPS": onePoint("2026-08-10T00:00:00Z", 80, 15),
	})
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "payments-prod", Class: "db.t3.large"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBIOPS, start, start.Add(15*time.Minute), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].P95 != 200 {
		t.Fatalf("ReadIOPS+WriteIOPS must sum: %+v", bs)
	}
}

// TestSeriesMemPercent uses hand-computed values: db.t3.large has 8 GiB
// of memory. 7 GiB freeable → 100 × (1 − 7/8) = 12.5%. Oversized
// freeable (> total) clamps to 0; negative freeable clamps to 100.
func TestSeriesMemPercent(t *testing.T) {
	gib := float64(1 << 30)
	srv, _ := cwServer(t, map[string]string{
		"FreeableMemory": onePoint("2026-08-10T00:00:00Z", 7*gib, 15),
	})
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "payments-prod", Class: "db.t3.large"} // 8 GiB
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBMemPercent, start, start.Add(15*time.Minute), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 {
		t.Fatalf("mem buckets: %d", len(bs))
	}
	if got := bs[0].P95; got != 12.5 {
		t.Fatalf("mem percent: want 12.5 (7/8 GiB free), got %v", got)
	}

	// Clamp: 9 GiB freeable of 8 GiB total → negative → 0.
	srv2, _ := cwServer(t, map[string]string{
		"FreeableMemory": onePoint("2026-08-10T00:00:00Z", 9*gib, 15),
	})
	defer srv2.Close()
	s2 := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv2.URL, client: srv2.Client()}
	bs, err = s2.Series(context.Background(), inst, store.MetricDBMemPercent, start, start.Add(15*time.Minute), 15*time.Minute)
	if err != nil || len(bs) != 1 {
		t.Fatalf("clamp low: %v %v", len(bs), err)
	}
	if bs[0].P95 != 0 {
		t.Fatalf("clamp low: want 0, got %v", bs[0].P95)
	}

	// Clamp high: negative freeable (a known RDS quirk on small
	// instances) → over 100 → 100.
	srv3, _ := cwServer(t, map[string]string{
		"FreeableMemory": onePoint("2026-08-10T00:00:00Z", -1024, 15),
	})
	defer srv3.Close()
	s3 := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv3.URL, client: srv3.Client()}
	bs, err = s3.Series(context.Background(), inst, store.MetricDBMemPercent, start, start.Add(15*time.Minute), 15*time.Minute)
	if err != nil || len(bs) != 1 {
		t.Fatalf("clamp high: %v %v", len(bs), err)
	}
	if bs[0].P95 != 100 {
		t.Fatalf("clamp high: want 100, got %v", bs[0].P95)
	}
}

func TestSeriesMemUnknownClassIsEmpty(t *testing.T) {
	srv, _ := cwServer(t, map[string]string{})
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "odd", Class: "db.future.mega"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBMemPercent, start, start.Add(time.Hour), 15*time.Minute)
	if err != nil || len(bs) != 0 {
		t.Fatalf("unknown class must yield no mem evidence: %d err=%v", len(bs), err)
	}
}

func TestSeriesErrorsIsEmptyNoEvidence(t *testing.T) {
	srv, _ := cwServer(t, map[string]string{})
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "payments-prod", Class: "db.t3.large"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBErrors, start, start.Add(24*time.Hour), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 0 {
		t.Fatalf("db_errors must be empty (CloudWatch has no RDS error counter): %d", len(bs))
	}
}

func TestSeriesUnknownMetricIsEmpty(t *testing.T) {
	srv, _ := cwServer(t, map[string]string{})
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "payments-prod", Class: "db.t3.large"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, "not_a_metric", start, start.Add(time.Hour), 15*time.Minute)
	if err != nil || len(bs) != 0 {
		t.Fatalf("unknown metric: %d err=%v", len(bs), err)
	}
}

// TestSeriesChunksLongWindows: a 30h window must split into a 24h chunk
// plus a 6h chunk (GetMetricStatistics caps the range at 86,400s), each
// with step-aligned StartTime and the right Period.
func TestSeriesChunksLongWindows(t *testing.T) {
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	srv, bodies := cwServer(t, map[string]string{
		"CPUUtilization": `{"Label":"CPUUtilization","Datapoints":[
			{"Timestamp":"2026-08-11T00:00:00Z","Average":5,"SampleCount":1}]}`,
	})
	defer srv.Close()
	s := &Source{region: "us-east-1", rdsBase: "http://unused", cwBase: srv.URL, client: srv.Client()}

	inst := dbmetrics.Instance{Name: "payments-prod", Class: "db.t3.large"}
	bs, err := s.Series(context.Background(), inst, store.MetricDBCPUPercent,
		start, start.Add(30*time.Hour), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].P95 != 5 {
		t.Fatalf("chunked series: %+v", bs)
	}
	if got := *bodies; len(got) != 2 {
		t.Fatalf("want 2 chunks, got %d: %v", len(got), got)
	}
	for i, body := range *bodies {
		var req struct {
			StartTime string              `json:"StartTime"`
			EndTime   string              `json:"EndTime"`
			Period    int                 `json:"Period"`
			Namespace string              `json:"Namespace"`
			Stats     []string            `json:"Statistics"`
			Dims      []map[string]string `json:"Dimensions"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if req.Namespace != "AWS/RDS" || req.Period != 900 ||
			len(req.Stats) != 1 || req.Stats[0] != "Average" ||
			len(req.Dims) != 1 || req.Dims[0]["Name"] != "DBInstanceIdentifier" || req.Dims[0]["Value"] != "payments-prod" {
			t.Fatalf("chunk %d request shape: %+v", i, req)
		}
		if !strings.HasPrefix(req.StartTime, "2026-08-10T00:00:00Z") && !strings.HasPrefix(req.StartTime, "2026-08-11T00:00:00Z") {
			t.Fatalf("chunk %d start: %q", i, req.StartTime)
		}
	}
	if !strings.HasPrefix((*bodies)[1], `{"Dimensions":[{"Name":"DBInstanceIdentifier","Value":"payments-prod"}],"EndTime":"2026-08-11T06:00:00Z"`) {
		t.Fatalf("second chunk must end at +30h: %s", (*bodies)[1])
	}
}

func TestNewSourceConfigAndDefaults(t *testing.T) {
	t.Setenv("CONSIZE_AWS_REGION", "eu-west-1")
	t.Setenv("CONSIZE_DB_FILTER", "a, b ,c")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRET")
	t.Setenv("AWS_SESSION_TOKEN", "TOKEN")
	s := NewSource()
	if s.region != "eu-west-1" || s.rdsBase != "https://rds.eu-west-1.amazonaws.com" ||
		s.cwBase != "https://monitoring.eu-west-1.amazonaws.com" {
		t.Fatalf("region/endpoints: %+v", s)
	}
	if len(s.filter) != 3 || s.filter[0] != "a" || s.filter[1] != "b" || s.filter[2] != "c" {
		t.Fatalf("filter: %v", s.filter)
	}
	if s.accessKey != "AKID" || s.secretKey != "SECRET" || s.sessionToken != "TOKEN" {
		t.Fatalf("creds not mirrored from env: %+v", s)
	}

	t.Setenv("CONSIZE_AWS_REGION", "")
	t.Setenv("CONSIZE_DB_FILTER", "")
	s = NewSource()
	if s.region != "us-east-1" || len(s.filter) != 0 {
		t.Fatalf("defaults: %+v", s)
	}
}
