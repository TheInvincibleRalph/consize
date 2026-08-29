package analysis

import (
	"math"
	"strings"
	"testing"
)

const (
	testExcludeKey = "consize.savings.dev/exclude"
	testRiskKey    = "consize.savings.dev/data-loss-risk"
)

// fixture returns a workload whose usage is exactly known: 5 days × 10
// buckets/day, every sample 120 MiB / 300 millicores. Daily p95 = 120 MiB,
// window p95 = p99 = 120 MiB — every expected number below is hand-computed.
func fixture(dayValues [][]int64) Workload {
	w := Workload{
		Name:       "golden",
		Namespace:  "apps",
		Kind:       "deployment",
		RequestMem: 8192 * MiB,
		LimitMem:   16 * GiB,
		RequestCPU: 2000,
		LimitCPU:   4000,
		Labels:     map[string]string{"app": "golden"},
	}
	start := int64(1750000000)
	for d, vals := range dayValues {
		for b, v := range vals {
			w.Buckets = append(w.Buckets, Bucket{
				MemUsedBytes: v * MiB,
				CPUUsedMilli: 300, // constant CPU usage
				WindowStart:  start + int64(d*86400+b*900),
			})
		}
	}
	return w
}

// TestGoldenCompute is the shipped golden fixture: the analysis engine's
// output for a known input is asserted to the byte and to the cent.
//
// Hand-computed expectations:
//
//	p95 of daily p95s (120 MiB × 5 days)            = 120 MiB
//	request = ceil(120 × 1.2)                       = 144 MiB
//	limit   = max(2 × 144, 120)                     = 288 MiB
//	memory savings = (8192 − 144)/1024 GiB × $3.66  = $28.7653125
//	cpu request = ceil(300 × 1.2)                   = 360 millicores
//	limit       = max(2 × 360, 300)                 = 720 millicores
//	cpu savings  = (2000 − 360)/1000 × $27.40       = $44.936
func TestGoldenCompute(t *testing.T) {
	days := make([][]int64, 5)
	for d := range days {
		days[d] = make([]int64, 10)
		for b := range days[d] {
			days[d][b] = 120
		}
	}
	res := Analyze([]Workload{fixture(days)}, DefaultPrices())

	if len(res.Recommendations) != 2 {
		t.Fatalf("want 2 recommendations (memory + cpu), got %d: %+v", len(res.Recommendations), res.Recommendations)
	}
	byRes := map[string]Recommendation{}
	for _, r := range res.Recommendations {
		byRes[r.Resource] = r
	}

	mem, ok := byRes[ResourceMemory]
	if !ok {
		t.Fatal("missing memory recommendation")
	}
	if mem.Current != 8192*MiB {
		t.Errorf("memory current = %d, want 8192 MiB", mem.Current)
	}
	if mem.Recommended != 144*MiB {
		t.Errorf("memory request = %d bytes, want %d (144 MiB)", mem.Recommended, 144*MiB)
	}
	// limit = max(2 × 144, 120) = 288 MiB; current limit 16 GiB.
	if mem.LimitCurrent != 16*GiB || mem.LimitProposed != 288*MiB {
		t.Errorf("memory limit = %d → %d, want %d → %d", mem.LimitCurrent, mem.LimitProposed, 16*GiB, 288*MiB)
	}
	if got := mem.SavingsMonth; math.Abs(got-28.7653125) > 1e-9 {
		t.Errorf("memory savings = %.9f, want 28.7653125", got)
	}

	cpu, ok := byRes[ResourceCPU]
	if !ok {
		t.Fatal("missing cpu recommendation")
	}
	if cpu.Current != 2000 {
		t.Errorf("cpu current = %d, want 2000", cpu.Current)
	}
	if cpu.Recommended != 360 {
		t.Errorf("cpu request = %d milli, want 360", cpu.Recommended)
	}
	// limit = max(2 × 360, 300) = 720 millicores; current limit 4000.
	if cpu.LimitCurrent != 4000 || cpu.LimitProposed != 720 {
		t.Errorf("cpu limit = %d → %d, want 4000 → 720", cpu.LimitCurrent, cpu.LimitProposed)
	}
	if got := cpu.SavingsMonth; math.Abs(got-44.936) > 1e-9 {
		t.Errorf("cpu savings = %.9f, want 44.936", got)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("unexpected skips: %+v", res.Skipped)
	}
}

// TestSpikeNotBlind asserts the percentile aggregation is burst-resistant:
// one spike day must not size the request to the spike — the request stays
// near typical usage while the limit absorbs the worst day.
func TestSpikeNotBlind(t *testing.T) {
	days := make([][]int64, 14)
	for d := range days {
		days[d] = make([]int64, 10)
		for b := range days[d] {
			days[d][b] = 400 // 400 MiB base
		}
	}
	for b := range days[7] {
		days[7][b] = 3200 // spike day: 8×
	}
	res := Analyze([]Workload{fixture(days)}, DefaultPrices())

	var mem *Recommendation
	for i := range res.Recommendations {
		if res.Recommendations[i].Resource == ResourceMemory {
			mem = &res.Recommendations[i]
		}
	}
	if mem == nil {
		t.Fatal("expected a memory recommendation")
	}
	reqMiB := mem.Recommended / MiB
	if reqMiB >= 3200 {
		t.Errorf("request %d MiB sized to the spike — should stay below 3200", reqMiB)
	}
	if reqMiB < 400 {
		t.Errorf("request %d MiB below base usage — should cover typical usage", reqMiB)
	}
	if mem.Recommended > mem.Current {
		t.Errorf("downsize-only violated: request %d > current %d", mem.Recommended, mem.Current)
	}
}

// TestSkipConditions covers every policy skip path.
func TestSkipConditions(t *testing.T) {
	days := make([][]int64, 5)
	for d := range days {
		days[d] = make([]int64, 10)
		for b := range days[d] {
			days[d][b] = 120
		}
	}

	cases := []struct {
		name   string
		mutate func(*Workload)
		reason string
	}{
		{
			"excluded by label",
			func(w *Workload) { w.Labels[testExcludeKey] = "true" },
			"excluded",
		},
		{
			"protected namespace",
			func(w *Workload) { w.Namespace = "kube-system" },
			"protected namespace",
		},
		{
			"data-loss-risk",
			func(w *Workload) { w.Labels[testRiskKey] = "true" },
			"data-loss-risk",
		},
		{
			"insufficient data",
			func(w *Workload) { w.Buckets = w.Buckets[:3*10] }, // 3 of 5 days
			"insufficient data",
		},
		{
			"already optimal",
			func(w *Workload) { // both resources: p95×1.2 ≥ current
				w.RequestMem = 120 * MiB
				w.RequestCPU = 300
			},
			"already optimal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := fixture(days)
			w.Labels = map[string]string{} // fresh labels per case
			tc.mutate(&w)

			res := Analyze([]Workload{w}, DefaultPrices())
			if len(res.Recommendations) != 0 {
				t.Fatalf("expected no recommendations, got %+v", res.Recommendations)
			}
			if len(res.Skipped) != 1 {
				t.Fatalf("expected exactly one skip, got %+v", res.Skipped)
			}
			if got := res.Skipped[0].Reason; !strings.Contains(got, tc.reason) {
				t.Errorf("skip reason %q does not mention %q", got, tc.reason)
			}
		})
	}
}

// TestMinDataDaysKnob asserts the data-minimum is configurable (ADR-024):
// a 3-day workload passes a lowered minimum and is skipped by the shipped
// default, with the skip reason naming the threshold.
func TestMinDataDaysKnob(t *testing.T) {
	days := make([][]int64, 3)
	for d := range days {
		days[d] = make([]int64, 10)
		for b := range days[d] {
			days[d][b] = 120
		}
	}
	w := fixture(days) // 3 distinct days of data

	// Relaxed minimum: any data within the window suffices → sized.
	res := AnalyzeCfg([]Workload{w}, DefaultPrices(), Config{MinDataDays: 0.1})
	if len(res.Recommendations) != 2 {
		t.Fatalf("min-days 0.1: want 2 recommendations, got %d (skipped: %+v)", len(res.Recommendations), res.Skipped)
	}

	// Shipped default: 3 < 5 → skipped, reason names both numbers.
	res = AnalyzeCfg([]Workload{w}, DefaultPrices(), Config{MinDataDays: 5})
	if len(res.Recommendations) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("min-days 5: want 1 skip and 0 recommendations, got recs=%d skipped=%+v",
			len(res.Recommendations), res.Skipped)
	}
	if got := res.Skipped[0].Reason; !strings.Contains(got, "insufficient data (3/5 days)") {
		t.Fatalf("skip reason = %q, want it to name 3/5 days", got)
	}
}

// TestPercentileBasics checks the percentile primitive.
func TestPercentileBasics(t *testing.T) {
	vals := []int64{1, 2, 3, 4}
	if got := Percentile(vals, 50); got != 2.5 {
		t.Errorf("p50 = %v, want 2.5", got)
	}
	if got := Percentile(vals, 100); got != 4 {
		t.Errorf("p100 = %v, want 4", got)
	}
	if got := Percentile([]int64{7}, 95); got != 7 {
		t.Errorf("single value p95 = %v, want 7", got)
	}
	if got := Percentile([]int64{}, 95); got != 0 {
		t.Errorf("empty p95 = %v, want 0", got)
	}
}
