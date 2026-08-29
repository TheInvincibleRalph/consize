package api

import (
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"consize/internal/analysis"
	"consize/internal/store"
)

// contractMetrics is the frontend chart contract: the five metric names
// accepted by the series endpoint regardless of surface. Anything else
// is a 400.
var contractMetrics = map[string]bool{
	"cpu_percent": true,
	"mem_percent": true,
	"iops":        true,
	"connections": true,
	"errors":      true,
}

// seriesMetric resolves the API's metric names (the frontend chart
// contract) to a store bucket metric for the workload's surface. DB
// workloads (Source "db") store the db_* metrics of ADR-030; compute
// workloads store the k8s raw metrics (millicores / bytes). The unit is
// what the chart axis labels: percent / iops / connections / errors for
// databases, millicores / bytes for compute. A name that is valid in
// the contract but does not exist on this surface (e.g. iops for a
// compute workload) resolves to ("", "") — no data, like a source that
// doesn't emit the metric; the handler answers 200 with empty points.
func seriesMetric(wl *store.Workload, name string) (storeMetric, unit string) {
	if wl.Source == "db" {
		switch name {
		case "cpu_percent":
			return store.MetricDBCPUPercent, "percent"
		case "mem_percent":
			return store.MetricDBMemPercent, "percent"
		case "iops":
			return store.MetricDBIOPS, "iops"
		case "connections":
			return store.MetricDBConnections, "connections"
		case "errors":
			return store.MetricDBErrors, "errors"
		}
		return "", ""
	}
	switch name {
	case "cpu_percent":
		return store.MetricCPUMilli, "millicores"
	case "mem_percent":
		return store.MetricMemBytes, "bytes"
	}
	return "", ""
}

// seriesPoint is one daily bucket of a metric series: the day's
// p50/p95/p99/max of the 15-minute window values (P95 per bucket, the
// engine's window value), computed with the analysis percentile math.
type seriesPoint struct {
	TS  string  `json:"ts"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// workloadSeries serves GET /api/v1/workloads/{id}/series?metric=&days=:
// one metric's daily buckets over the trailing window, aggregated from
// usage_buckets exactly like the analysis engine aggregates (per-day
// percentile of the 15-minute p95s, then percentiles over the days).
// 404 unknown workload; 400 unknown metric or invalid days.
func (s *Server) workloadSeries(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workload id must be an integer"})
		return
	}
	wl, err := s.store.GetWorkload(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workload not found"})
			return
		}
		writeErr(w, err)
		return
	}
	metric := r.URL.Query().Get("metric")
	if _, ok := contractMetrics[metric]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unknown metric " + strconv.Quote(metric) +
				" (allowed: cpu_percent, mem_percent, iops, connections, errors)"})
		return
	}
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be a positive integer"})
			return
		}
		days = n
	}
	bucketMetric, unit := seriesMetric(&wl, metric)
	if bucketMetric == "" {
		// Valid contract name, but the workload's surface has no such
		// metric (e.g. iops for a compute workload): empty series, not
		// an error — the same semantics as a source that never emits it.
		writeJSON(w, http.StatusOK, map[string]any{
			"workload_id": wl.ID,
			"metric":      metric,
			"days":        days,
			"unit":        "",
			"points":      []seriesPoint{},
		})
		return
	}
	end := time.Now().UTC()
	bs, err := s.store.ListBuckets(r.Context(), wl.ID, bucketMetric,
		end.Add(-time.Duration(days)*24*time.Hour), end)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workload_id": wl.ID,
		"metric":      metric,
		"days":        days,
		"unit":        unit,
		"points":      dailySeries(bs),
	})
}

// dailySeries aggregates buckets into one point per UTC day.
func dailySeries(bs []store.Bucket) []seriesPoint {
	byDay := map[int64][]float64{}
	for _, b := range bs {
		day := b.WindowStart.Unix() / 86400
		byDay[day] = append(byDay[day], b.P95)
	}
	days := make([]int64, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i] < days[j] })

	points := make([]seriesPoint, 0, len(days))
	for _, d := range days {
		vals := byDay[d]
		points = append(points, seriesPoint{
			TS:  time.Unix(d*86400, 0).UTC().Format(time.RFC3339),
			P50: round1(analysis.Percentile(vals, 50)),
			P95: round1(analysis.Percentile(vals, 95)),
			P99: round1(analysis.Percentile(vals, 99)),
			Max: round1(maxOf(vals)),
		})
	}
	return points
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func maxOf(vals []float64) float64 {
	m := 0.0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}
