package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"consize/internal/api"
	"consize/internal/pricing"
	"consize/internal/store"
)

func newTestServerM35(t *testing.T) (http.Handler, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	return api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil, nil), st
}

// --- GET /api/v1/workloads/{id}/series ---

// seedSeriesBucket writes one 15-minute bucket with the given p95.
func seedSeriesBucket(t *testing.T, st store.Store, wlID int64, metric string, ts time.Time, p95 float64) {
	t.Helper()
	if err := st.UpsertBucket(context.Background(), store.Bucket{
		WorkloadID: wlID, Metric: metric, WindowStart: ts,
		P50: p95, P95: p95, P99: p95, Max: p95, Samples: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSeriesEndpointHappyPath(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)
	wlID, err := st.UpsertWorkload(ctx, store.Workload{Name: "payments-prod", Namespace: "db", Source: "db", DBClass: "db.t3.large"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(15 * time.Minute)
	// Day 1: p95s 10 and 20 → p50 15, p95 19.5, p99 19.9, max 20.
	seedSeriesBucket(t, st, wlID, store.MetricDBCPUPercent, now.Add(-24*time.Hour), 10)
	seedSeriesBucket(t, st, wlID, store.MetricDBCPUPercent, now.Add(-24*time.Hour).Add(15*time.Minute), 20)
	// Day 2: p95s 30 and 40 → p50 35, p95 39.5, p99 39.9, max 40.
	seedSeriesBucket(t, st, wlID, store.MetricDBCPUPercent, now.Add(-48*time.Hour), 30)
	seedSeriesBucket(t, st, wlID, store.MetricDBCPUPercent, now.Add(-48*time.Hour).Add(15*time.Minute), 40)

	rec := get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric=cpu_percent")
	if rec.Code != http.StatusOK {
		t.Fatalf("series: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		WorkloadID int64  `json:"workload_id"`
		Metric     string `json:"metric"`
		Days       int    `json:"days"`
		Points     []struct {
			TS  string  `json:"ts"`
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
			P99 float64 `json:"p99"`
			Max float64 `json:"max"`
		} `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.WorkloadID != wlID || body.Metric != "cpu_percent" || body.Days != 14 {
		t.Fatalf("series meta: %+v", body)
	}
	if len(body.Points) != 2 {
		t.Fatalf("want 2 daily points, got %d: %s", len(body.Points), rec.Body.String())
	}
	// Points are in calendar-day order (oldest first); the seeded days
	// are 24h apart so their calendar order is the seeding age order.
	want := map[string]struct{ p50, p95, p99, max float64 }{
		"day1": {15, 19.5, 19.9, 20}, // p95s [10, 20]
		"day2": {35, 39.5, 39.9, 40}, // p95s [30, 40]
	}
	seen := map[float64]struct{ p50, p95, p99, max float64 }{}
	for _, p := range body.Points {
		if _, err := time.Parse(time.RFC3339, p.TS); err != nil {
			t.Fatalf("ts must be RFC3339: %q", p.TS)
		}
		if !strings.HasSuffix(p.TS, "T00:00:00Z") {
			t.Fatalf("daily point must be UTC midnight: %q", p.TS)
		}
		seen[p.P50] = struct{ p50, p95, p99, max float64 }{p.P50, p.P95, p.P99, p.Max}
	}
	for name, w := range want {
		got, ok := seen[w.p50]
		if !ok || got != w {
			t.Fatalf("%s point: want %+v, got %+v (all: %+v)", name, w, got, seen)
		}
	}
}

func TestSeriesEndpointNoDataIsEmptyNotError(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)
	wlID, _ := st.UpsertWorkload(ctx, store.Workload{Name: "cold", Namespace: "db", Source: "db"})

	rec := get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric=connections")
	if rec.Code != http.StatusOK {
		t.Fatalf("no-data series must be 200 with empty points: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"points":[]`) {
		t.Fatalf("points must be an empty array, not null: %s", rec.Body.String())
	}
}

func TestSeriesEndpointComputeSurface(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)
	wlID, err := st.UpsertWorkload(ctx, store.Workload{Name: "frontend", Namespace: "boutique", Source: "k8s", Kind: "deployment"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(15 * time.Minute)
	// Compute stores raw k8s metrics: cpu_used_milli. Day 1 p95s 100 and
	// 200 → p50 150, p95 195, p99 199, max 200.
	seedSeriesBucket(t, st, wlID, store.MetricCPUMilli, now.Add(-24*time.Hour), 100)
	seedSeriesBucket(t, st, wlID, store.MetricCPUMilli, now.Add(-24*time.Hour).Add(15*time.Minute), 200)

	rec := get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric=cpu_percent")
	if rec.Code != http.StatusOK {
		t.Fatalf("compute series: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Unit   string `json:"unit"`
		Points []struct {
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
		} `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Unit != "millicores" {
		t.Fatalf("compute cpu unit: want millicores, got %q", body.Unit)
	}
	if len(body.Points) != 1 || body.Points[0].P50 != 150 || body.Points[0].P95 != 195 {
		t.Fatalf("compute cpu points: want p50 150 / p95 195, got %+v", body.Points)
	}

	// mem_percent on compute → bytes, from mem_used_bytes.
	seedSeriesBucket(t, st, wlID, store.MetricMemBytes, now.Add(-24*time.Hour).Add(30*time.Minute), 1048576)
	rec = get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric=mem_percent")
	var mem struct {
		Unit   string `json:"unit"`
		Points []struct {
			P50 float64 `json:"p50"`
		} `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mem); err != nil {
		t.Fatal(err)
	}
	if mem.Unit != "bytes" || len(mem.Points) != 1 || mem.Points[0].P50 != 1048576 {
		t.Fatalf("compute mem: unit %q points %+v", mem.Unit, mem.Points)
	}

	// iops is in the contract but compute has no such metric: 200 with
	// empty points and an empty unit — no-data, not an error.
	rec = get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric=iops")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"points":[]`) {
		t.Fatalf("compute iops must be 200 with empty points: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"unit":""`) {
		t.Fatalf("compute iops unit must be empty: %s", rec.Body.String())
	}
}

func TestSeriesEndpointDBUnit(t *testing.T) {
	// The DB surface reports percent units for cpu/mem.
	ctx := context.Background()
	h, st := newTestServerM35(t)
	wlID, _ := st.UpsertWorkload(ctx, store.Workload{Name: "payments-prod", Namespace: "db", Source: "db", DBClass: "db.t3.large"})
	now := time.Now().UTC().Truncate(15 * time.Minute)
	seedSeriesBucket(t, st, wlID, store.MetricDBCPUPercent, now.Add(-24*time.Hour), 10)

	for _, m := range []string{"cpu_percent", "mem_percent"} {
		rec := get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric="+m)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"unit":"percent"`) {
			t.Fatalf("db %s: want 200 unit percent, got %d %s", m, rec.Code, rec.Body.String())
		}
	}
}

func TestSeriesEndpointErrors(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)
	wlID, _ := st.UpsertWorkload(ctx, store.Workload{Name: "payments-prod", Namespace: "db", Source: "db"})

	// 404 unknown workload.
	if rec := get(t, h, "/api/v1/workloads/99999/series?metric=cpu_percent"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown workload: want 404, got %d", rec.Code)
	}
	// 400 bad id.
	if rec := get(t, h, "/api/v1/workloads/abc/series?metric=cpu_percent"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: want 400, got %d", rec.Code)
	}
	// 400 unknown metric.
	for _, m := range []string{"banana", "cpu_used_milli", ""} {
		if rec := get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric="+m); rec.Code != http.StatusBadRequest {
			t.Fatalf("metric %q: want 400, got %d", m, rec.Code)
		}
	}
	// 400 invalid days.
	for _, d := range []string{"0", "-1", "abc"} {
		if rec := get(t, h, "/api/v1/workloads/"+strconv.FormatInt(wlID, 10)+"/series?metric=cpu_percent&days="+d); rec.Code != http.StatusBadRequest {
			t.Fatalf("days=%q: want 400, got %d", d, rec.Code)
		}
	}
}

// --- GET /api/v1/savings: realized + by_owner ---

// seedEvents writes apply events for a recommendation in order; the last
// one is the latest. Events are created in the given order — the store
// stamps CreatedAt from the wall clock, so sequential events get a brief
// pause to make the ordering observable (back-to-back calls can share a
// clock tick).
func seedEvents(t *testing.T, st store.Store, recID, wlID int64, results ...string) {
	t.Helper()
	for i, res := range results {
		if i > 0 {
			time.Sleep(2 * time.Millisecond)
		}
		if _, err := st.CreateApplyEvent(context.Background(), store.ApplyEvent{
			RecommendationID: recID, WorkloadID: wlID, Actor: "alice", Mode: "approved",
			Result: res,
			Diff:   store.Diff{Resource: store.ResourceClass},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func seedAppliedWithVerification(t *testing.T, st store.Store, recID, wlID int64, verdict string) int64 {
	t.Helper()
	applyID, err := st.CreateApplyEvent(context.Background(), store.ApplyEvent{
		RecommendationID: recID,
		WorkloadID:       wlID,
		Actor:            "alice",
		Mode:             "approved",
		Result:           store.EventApplied,
		Diff:             store.Diff{Resource: store.ResourceClass},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateVerificationRun(context.Background(), store.VerificationRun{
		ApplyEventID: applyID,
		Verdict:      verdict,
	}); err != nil {
		t.Fatal(err)
	}
	return applyID
}

// recIDFor finds a recommendation's ID by (workload, savings) — the
// memory store shares one ID counter across workloads and
// recommendations, so IDs are not predictable.
func recIDFor(t *testing.T, st store.Store, wlID int64, savings float64) int64 {
	t.Helper()
	recs, _, err := st.ListRecommendations(context.Background(), &wlID, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.SavingsMonthly == savings {
			return r.ID
		}
	}
	t.Fatalf("no recommendation for workload %d with savings %v", wlID, savings)
	return 0
}

func TestSavingsRealizedAndByOwner(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)

	w1, _ := st.UpsertWorkload(ctx, store.Workload{Name: "payments", Namespace: "db", Source: "db", Labels: map[string]string{"owner": "payments-team"}})
	w2, _ := st.UpsertWorkload(ctx, store.Workload{Name: "analytics", Namespace: "db", Source: "db", Labels: map[string]string{"owner": "analytics-team"}})
	w3, _ := st.UpsertWorkload(ctx, store.Workload{Name: "legacy", Namespace: "db", Source: "db"}) // no owner label

	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: w1, Resource: store.ResourceClass, ClassCurrent: "db.t3.large", ClassProposed: "db.t3.medium", SavingsMonthly: 30, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: w1, Resource: store.ResourceClass, ClassCurrent: "db.t3.large", ClassProposed: "db.t3.small", SavingsMonthly: 40, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: w2, Resource: store.ResourceClass, ClassCurrent: "db.t3.large", ClassProposed: "db.t3.medium", SavingsMonthly: 50, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: w3, Resource: store.ResourceClass, ClassCurrent: "db.t3.medium", ClassProposed: "db.t3.small", SavingsMonthly: 10, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}
	seedAppliedWithVerification(t, st, recIDFor(t, st, w1, 30), w1, store.VerdictPassed)       // realized 30
	seedEvents(t, st, recIDFor(t, st, w1, 40), w1, store.EventApplied, store.EventReverted)    // reverted: never counted
	seedAppliedWithVerification(t, st, recIDFor(t, st, w2, 50), w2, store.VerdictPassed)       // realized 50
	seedAppliedWithVerification(t, st, recIDFor(t, st, w3, 10), w3, store.VerdictInconclusive) // inconclusive: never counted

	rec := get(t, h, "/api/v1/savings")
	if rec.Code != http.StatusOK {
		t.Fatalf("savings: %d", rec.Code)
	}
	var body struct {
		ProjectedMonthly float64 `json:"projected_monthly_savings"`
		Active           int     `json:"active_recommendations"`
		RealizedMonthly  float64 `json:"realized_monthly"`
		RealizedYearly   float64 `json:"realized_yearly"`
		ByOwner          map[string]struct {
			ProjectedMonthly float64 `json:"projected_monthly"`
			RealizedMonthly  float64 `json:"realized_monthly"`
		} `json:"by_owner"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ProjectedMonthly != 130 || body.Active != 4 {
		t.Fatalf("projected figures must be unchanged: %+v", body)
	}
	// Realized: 30 (applied) + 50 (applied); the reverted 40 and the
	// planned 10 never count.
	if body.RealizedMonthly != 80 {
		t.Fatalf("realized_monthly: want 80, got %v", body.RealizedMonthly)
	}
	if body.RealizedYearly != 960 {
		t.Fatalf("realized_yearly: want 960, got %v", body.RealizedYearly)
	}
	payments := body.ByOwner["payments-team"]
	if payments.ProjectedMonthly != 70 || payments.RealizedMonthly != 30 {
		t.Fatalf("payments-team: %+v", payments)
	}
	analytics := body.ByOwner["analytics-team"]
	if analytics.ProjectedMonthly != 50 || analytics.RealizedMonthly != 50 {
		t.Fatalf("analytics-team: %+v", analytics)
	}
	unassigned := body.ByOwner["unassigned"]
	if unassigned.ProjectedMonthly != 10 || unassigned.RealizedMonthly != 0 {
		t.Fatalf("unassigned: %+v", unassigned)
	}
}

// --- GET /api/v1/recommendations: risk flags ---

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// windowMinutes renders minutes-since-Sunday as ddd:hh:mm.
func windowMinutes(mins int) string {
	return fmt.Sprintf("%s:%02d:%02d", dayNames[(mins/1440)%7], (mins%1440)/60, mins%60)
}

// openWindowNow builds a maintenance window that contains now.
func openWindowNow(now time.Time) string {
	mow := int(now.Weekday())*1440 + now.Hour()*60 + now.Minute()
	return fmt.Sprintf("%s-%s", windowMinutes(mow), windowMinutes(mow+30))
}

// closedWindowNow builds a maintenance window that does NOT contain now.
func closedWindowNow(now time.Time) string {
	mow := int(now.Weekday())*1440 + now.Hour()*60 + now.Minute()
	return fmt.Sprintf("%s-%s", windowMinutes((mow+90)%(7*1440)), windowMinutes((mow+120)%(7*1440)))
}

// seedCpuDays writes buckets with the given p95 for n distinct days
// ending yesterday.
func seedCpuDays(t *testing.T, st store.Store, wlID int64, n int, metric string, p95 float64) {
	t.Helper()
	now := time.Now().UTC().Truncate(15 * time.Minute)
	for i := 1; i <= n; i++ {
		seedSeriesBucket(t, st, wlID, metric, now.Add(-time.Duration(i)*24*time.Hour), p95)
	}
}

type recView struct {
	store.Recommendation
	Risk        string   `json:"risk"`
	RiskReasons []string `json:"risk_reasons"`
}

func getRecs(t *testing.T, h http.Handler) []recView {
	t.Helper()
	rec := get(t, h, "/api/v1/recommendations")
	if rec.Code != http.StatusOK {
		t.Fatalf("recommendations: %d", rec.Code)
	}
	var body struct {
		Recommendations []recView `json:"recommendations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Recommendations
}

func hasReason(rs []string, needle string) bool {
	for _, r := range rs {
		if strings.Contains(r, needle) {
			return true
		}
	}
	return false
}

func TestRecommendationRiskClassFlags(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)

	// Multi-step (xlarge → micro = 4 classes) + closed maintenance window.
	w1, _ := st.UpsertWorkload(ctx, store.Workload{
		Name: "step", Namespace: "db", Source: "db",
		DBClass: "db.t3.xlarge", DBMaintenanceWindow: closedWindowNow(time.Now().UTC()),
	})
	seedCpuDays(t, st, w1, 10, store.MetricDBCPUPercent, 10)
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: w1, Resource: store.ResourceClass, ClassCurrent: "db.t3.xlarge", ClassProposed: "db.t3.micro",
			SavingsMonthly: 185, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	views := getRecs(t, h)
	if len(views) != 1 {
		t.Fatalf("want 1 rec, got %d", len(views))
	}
	v := views[0]
	if v.Risk != "medium" {
		t.Fatalf("risk level: want medium, got %q (%v)", v.Risk, v.RiskReasons)
	}
	if !hasReason(v.RiskReasons, "class step spans 4 catalog classes") {
		t.Fatalf("step-distance reason missing: %v", v.RiskReasons)
	}
	if !hasReason(v.RiskReasons, "maintenance window not yet open") {
		t.Fatalf("window reason missing: %v", v.RiskReasons)
	}
	// Existing fields still ride the payload.
	if v.ClassCurrent != "db.t3.xlarge" || v.ClassProposed != "db.t3.micro" || v.SavingsMonthly != 185 {
		t.Fatalf("recommendation payload altered: %+v", v)
	}
}

func TestRecommendationRiskLow(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)

	// One-step downsize, open window, plenty of quiet data → low.
	w, _ := st.UpsertWorkload(ctx, store.Workload{
		Name: "healthy", Namespace: "db", Source: "db",
		DBClass: "db.t3.large", DBMaintenanceWindow: openWindowNow(time.Now().UTC()),
	})
	seedCpuDays(t, st, w, 10, store.MetricDBCPUPercent, 10)
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: w, Resource: store.ResourceClass, ClassCurrent: "db.t3.large", ClassProposed: "db.t3.medium",
			SavingsMonthly: 50, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	v := getRecs(t, h)[0]
	if v.Risk != "low" {
		t.Fatalf("want low risk, got %q (%v)", v.Risk, v.RiskReasons)
	}
}

func TestRecommendationRiskDataLossHighAndLowData(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)

	// data-loss-risk wins everything → high; also only 2 data days.
	w1, _ := st.UpsertWorkload(ctx, store.Workload{
		Name: "risky", Namespace: "db", Source: "db",
		DBClass:             "db.t3.large",
		DBMaintenanceWindow: openWindowNow(time.Now().UTC()),
		Labels:              map[string]string{"consize.savings.dev/data-loss-risk": "true"},
	})
	seedCpuDays(t, st, w1, 2, store.MetricDBCPUPercent, 10)
	// Low data days alone → medium.
	w2, _ := st.UpsertWorkload(ctx, store.Workload{
		Name: "young", Namespace: "db", Source: "db",
		DBClass:             "db.t3.large",
		DBMaintenanceWindow: openWindowNow(time.Now().UTC()),
	})
	seedCpuDays(t, st, w2, 2, store.MetricDBCPUPercent, 10)
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: w1, Resource: store.ResourceClass, ClassCurrent: "db.t3.large", ClassProposed: "db.t3.medium", SavingsMonthly: 50, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: w2, Resource: store.ResourceClass, ClassCurrent: "db.t3.large", ClassProposed: "db.t3.medium", SavingsMonthly: 50, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	views := getRecs(t, h)
	if len(views) != 2 {
		t.Fatalf("recs: %d", len(views))
	}
	byName := map[string]recView{}
	for _, v := range views {
		byName[v.WorkloadName] = v
	}
	if v := byName["risky"]; v.Risk != "high" || !hasReason(v.RiskReasons, "data-loss-risk") {
		t.Fatalf("data-loss-risk must be high: %+v", v)
	}
	if v := byName["young"]; v.Risk != "medium" || !hasReason(v.RiskReasons, "low data days (2 of 5 required)") {
		t.Fatalf("low data days must be medium: %+v", v)
	}
}

func TestRecommendationRiskFollowUpAndSaturation(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)

	// A pending follow-up: its current class is NOT the workload's live
	// class (the live class is still the original after step 1).
	w1, _ := st.UpsertWorkload(ctx, store.Workload{
		Name: "stepped", Namespace: "db", Source: "db",
		DBClass:             "db.t3.xlarge",
		DBMaintenanceWindow: openWindowNow(time.Now().UTC()),
	})
	seedCpuDays(t, st, w1, 10, store.MetricDBCPUPercent, 10)
	// Near-headroom: each day gets four 55% buckets so the day p95 (linear
	// interpolation) is exactly 55 (cap 60, near at ≥ 50).
	now := time.Now().UTC().Truncate(15 * time.Minute)
	for i := 1; i <= 10; i++ {
		for m := 1; m <= 4; m++ {
			seedSeriesBucket(t, st, w1, store.MetricDBCPUPercent,
				now.Add(-time.Duration(i)*24*time.Hour).Add(time.Duration(m)*15*time.Minute), 55)
		}
	}
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: w1, Resource: store.ResourceClass, ClassCurrent: "db.t3.medium", ClassProposed: "db.t3.small",
			SavingsMonthly: 25, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	v := getRecs(t, h)[0]
	if v.Risk != "medium" {
		t.Fatalf("want medium, got %q", v.Risk)
	}
	if !hasReason(v.RiskReasons, "pending follow-up of a multi-step class plan") {
		t.Fatalf("follow-up reason missing: %v", v.RiskReasons)
	}
	if !hasReason(v.RiskReasons, "cpu p95 at 55.0% within 10 points of the 60% headroom cap") {
		t.Fatalf("saturation reason missing: %v", v.RiskReasons)
	}
}

func TestRecommendationRiskK8sUtilization(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServerM35(t)

	w, _ := st.UpsertWorkload(ctx, store.Workload{Name: "api", Namespace: "prod", Source: "k8s", RequestCPUMilli: 1000})
	// p95 900 milli = 90% of the 1000 request ≥ 80 → medium.
	seedCpuDays(t, st, w, 10, store.MetricCPUMilli, 900)
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: w, Resource: store.ResourceCPU, CurrentValue: 1000, ProposedValue: 600,
			SavingsMonthly: 10, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	v := getRecs(t, h)[0]
	if v.Risk != "medium" {
		t.Fatalf("want medium for thin-headroom k8s rec, got %q (%v)", v.Risk, v.RiskReasons)
	}
	if !hasReason(v.RiskReasons, "cpu utilization p95 at 90.0% of the current request") {
		t.Fatalf("utilization reason missing: %v", v.RiskReasons)
	}
}
