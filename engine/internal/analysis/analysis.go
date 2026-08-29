// Package analysis implements Consize's sizing policy: percentile-based
// recommendations for CPU and memory, exactly as designed in
// docs/architecture.md §3.2 (Analysis Engine).
//
// The functions here are pure — no I/O — so the entire policy is unit
// testable with golden fixtures.
package analysis

import (
	"fmt"
	"math"
	"sort"
)

// Resource kinds managed by Consize.
const (
	ResourceCPU    = "cpu"
	ResourceMemory = "memory"
)

// Policy defaults (docs/architecture.md §3.2).
const (
	// MinDataDays is the shipped default for the data-minimum confidence
	// gate (Config.MinDataDays, ADR-024).
	MinDataDays    = 5   // skip workloads with fewer days of data
	Headroom       = 1.2 // request = p95 × headroom
	MinLimitMult   = 2.0 // limit ≥ 2× request
	HoursPerMonth  = 730.0
	ConfidenceDays = 14 // full confidence needs a full window

	MiB = 1 << 20
	GiB = 1 << 30
)

// Config tunes the sizing policy. DefaultConfig is what ships; operators
// may relax the data-minimum (ADR-024). The verifier protects every apply,
// so the data-minimum is a confidence gate, not a safety gate.
type Config struct {
	// MinDataDays is the number of distinct days-with-data a workload
	// needs before it is sized. Fractional values are allowed (0.1 = any
	// data within the window).
	MinDataDays float64
}

// DefaultConfig returns the shipped policy settings.
func DefaultConfig() Config {
	return Config{MinDataDays: MinDataDays}
}

// Prices are the on-demand monthly rates used for savings math, mirroring
// the pricing service (§3.1) with GKE-style defaults.
type Prices struct {
	CPUPerCoreMonth float64
	MemPerGiBMonth  float64
}

// DefaultPrices returns the shipped default rate table.
func DefaultPrices() Prices {
	return Prices{CPUPerCoreMonth: 27.40, MemPerGiBMonth: 3.66}
}

// Workload is a managed resource in the cluster.
type Workload struct {
	Name       string
	Namespace  string
	Kind       string // deployment | statefulset
	Labels     map[string]string
	RequestCPU int64 // millicores
	LimitCPU   int64 // millicores
	RequestMem int64 // bytes
	LimitMem   int64 // bytes
	Buckets    []Bucket
}

// Bucket is one 15-minute usage sample.
type Bucket struct {
	CPUUsedMilli int64
	MemUsedBytes int64
	WindowStart  int64 // unix seconds; day = WindowStart / 86400
}

// Skipped records why a workload was not sized.
type Skipped struct {
	Workload  string
	Namespace string
	Reason    string
}

// Recommendation is one sized resource of one workload. The limit pair
// rides along with the request: the sizing policy computes both, and the
// apply engine patches request + limit together (limits only decrease in
// v1, so LimitProposed == LimitCurrent when the limit is unchanged).
type Recommendation struct {
	Workload      string
	Namespace     string
	Resource      string
	Current       int64
	Recommended   int64
	LimitCurrent  int64
	LimitProposed int64
	SavingsMonth  float64
	Confidence    float64 // 0..1, based on data volume
}

// Result bundles everything a run produced.
type Result struct {
	Recommendations []Recommendation
	Skipped         []Skipped
}

// Analyze runs the sizing policy with the shipped defaults. See
// AnalyzeCfg for the configurable variant.
func Analyze(workloads []Workload, prices Prices) Result {
	return AnalyzeCfg(workloads, prices, DefaultConfig())
}

// AnalyzeCfg runs the sizing policy over a workload set: skip conditions
// first, then per-resource sizing, then recommendations sorted by savings.
func AnalyzeCfg(workloads []Workload, prices Prices, cfg Config) Result {
	var res Result
	for _, w := range workloads {
		if reason := skipReason(w, cfg); reason != "" {
			res.Skipped = append(res.Skipped, Skipped{Workload: w.Name, Namespace: w.Namespace, Reason: reason})
			continue
		}
		var sized bool
		if r, ok := sizeMemory(w, prices); ok {
			res.Recommendations = append(res.Recommendations, r)
			sized = true
		}
		if r, ok := sizeCPU(w, prices); ok {
			res.Recommendations = append(res.Recommendations, r)
			sized = true
		}
		if !sized {
			res.Skipped = append(res.Skipped, Skipped{Workload: w.Name, Namespace: w.Namespace, Reason: "already optimal (downsize-only policy)"})
		}
	}
	sort.Slice(res.Recommendations, func(i, j int) bool {
		return res.Recommendations[i].SavingsMonth > res.Recommendations[j].SavingsMonth
	})
	return res
}

// skipReason returns why a workload must not be sized, or "" if eligible.
func skipReason(w Workload, cfg Config) string {
	if w.Labels["consize.savings.dev/exclude"] == "true" {
		return "excluded by label consize.savings.dev/exclude=true"
	}
	switch w.Namespace {
	case "kube-system", "consize-system":
		return "protected namespace " + w.Namespace
	}
	if w.Labels["consize.savings.dev/data-loss-risk"] == "true" {
		return "stateful workload flagged data-loss-risk"
	}
	if d := daysOfData(w.Buckets); float64(d) < cfg.MinDataDays {
		return fmt.Sprintf("insufficient data (%d/%s days)", d, daysFmt(cfg.MinDataDays))
	}
	return ""
}

// daysFmt renders the configurable minimum readably: whole values as
// integers ("5"), fractional values as decimals ("0.1").
func daysFmt(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// sizeMemory sizes the memory request/limit. Returns false when the
// workload is already optimal (downsize-only policy).
func sizeMemory(w Workload, prices Prices) (Recommendation, bool) {
	if len(w.Buckets) == 0 {
		return Recommendation{}, false
	}
	daily := dailyP95(w.Buckets, func(b Bucket) int64 { return b.MemUsedBytes })

	p95MiB := Percentile(daily, 95) / float64(MiB)
	reqMiB := int64(math.Ceil(p95MiB * Headroom))
	if reqMiB*MiB >= w.RequestMem {
		return Recommendation{}, false // downsize-only
	}

	p99MiB := math.Ceil(Percentile(daily, 99) / float64(MiB))
	limMiB := int64(math.Max(p99MiB, float64(reqMiB)*MinLimitMult))
	if max := w.LimitMem / MiB; limMiB > max {
		limMiB = max // limits only decrease in v1
	}

	saved := float64(w.RequestMem-reqMiB*MiB) / float64(GiB) * prices.MemPerGiBMonth
	return Recommendation{
		Workload:      w.Name,
		Namespace:     w.Namespace,
		Resource:      ResourceMemory,
		Current:       w.RequestMem,
		Recommended:   reqMiB * MiB,
		LimitCurrent:  w.LimitMem,
		LimitProposed: limMiB * MiB,
		SavingsMonth:  saved,
		Confidence:    confidence(w.Buckets),
	}, true
}

// sizeCPU sizes the CPU request/limit. Returns false when already optimal.
func sizeCPU(w Workload, prices Prices) (Recommendation, bool) {
	if len(w.Buckets) == 0 {
		return Recommendation{}, false
	}
	daily := dailyP95(w.Buckets, func(b Bucket) int64 { return b.CPUUsedMilli })

	p95 := Percentile(daily, 95)
	req := int64(math.Ceil(p95 * Headroom))
	if req >= w.RequestCPU {
		return Recommendation{}, false // downsize-only
	}

	p99 := math.Ceil(Percentile(daily, 99))
	lim := int64(math.Max(p99, float64(req)*MinLimitMult))
	if lim > w.LimitCPU {
		lim = w.LimitCPU // limits only decrease in v1
	}

	saved := float64(w.RequestCPU-req) / 1000.0 * prices.CPUPerCoreMonth
	return Recommendation{
		Workload:      w.Name,
		Namespace:     w.Namespace,
		Resource:      ResourceCPU,
		Current:       w.RequestCPU,
		Recommended:   req,
		LimitCurrent:  w.LimitCPU,
		LimitProposed: lim,
		SavingsMonth:  saved,
		Confidence:    confidence(w.Buckets),
	}, true
}

// confidence scores data volume: full 14-day window = 1.0.
func confidence(buckets []Bucket) float64 {
	d := float64(daysOfData(buckets)) / ConfidenceDays
	if d > 1 {
		return 1
	}
	return d
}

// dailyP95 reduces buckets to one p95-per-day series — the aggregation
// the architecture specifies (per-day p95, then window percentiles).
func dailyP95(buckets []Bucket, value func(Bucket) int64) []float64 {
	byDay := map[int64][]int64{}
	for _, b := range buckets {
		day := b.WindowStart / 86400
		byDay[day] = append(byDay[day], value(b))
	}
	days := make([]int64, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i] < days[j] })

	p95s := make([]float64, 0, len(days))
	for _, d := range days {
		p95s = append(p95s, Percentile(byDay[d], 95))
	}
	return p95s
}

// daysOfData counts distinct days with at least one bucket.
func daysOfData(buckets []Bucket) int {
	seen := map[int64]bool{}
	for _, b := range buckets {
		seen[b.WindowStart/86400] = true
	}
	return len(seen)
}

// Percentile returns the p-th percentile (0–100) of values using linear
// interpolation. An empty slice returns 0. It works on both integer and
// float series — the daily p95 aggregation produces float64s.
func Percentile[T int64 | float64](values []T, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	vs := make([]float64, len(values))
	for i, v := range values {
		vs[i] = float64(v)
	}
	sort.Float64s(vs)
	if len(vs) == 1 {
		return vs[0]
	}
	rank := p / 100 * float64(len(vs)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return vs[lo]
	}
	frac := rank - float64(lo)
	return vs[lo]*(1-frac) + vs[hi]*frac
}
