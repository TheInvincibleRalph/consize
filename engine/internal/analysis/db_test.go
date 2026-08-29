package analysis

import "testing"

// goldenDB builds a DB instance with 14 days of constant per-metric
// samples (one per day, so the daily p95 and window p95 both equal the
// given value, and confidence = 14/14 = 1.0). Every expected number
// below is hand-computed against DBCatalog (ADR-030 §4) and the
// projection model (§6).
func goldenDB(name, class string, cpu, mem, iops, conns float64) DBInstance {
	in := DBInstance{Name: name, Namespace: "db", Class: class}
	for day := 0; day < 14; day++ {
		ts := int64(day) * 86400
		in.Buckets = append(in.Buckets,
			DBBucket{Metric: DBMetricCPUPercent, WindowStart: ts, Value: cpu},
			DBBucket{Metric: DBMetricMemPercent, WindowStart: ts, Value: mem},
			DBBucket{Metric: DBMetricIOPS, WindowStart: ts, Value: iops},
			DBBucket{Metric: DBMetricConnections, WindowStart: ts, Value: conns},
		)
	}
	return in
}

// TestDBAnalyzeGolden is the ADR-030 worked example: t3.large
// (2 vCPU / 8 GiB / 2400 IOPS / 1600 conns / $100) measured at cpu 10%,
// mem 12.5% (1 GiB of 8), 200 IOPS, 300 connections.
//
//	         cpu     iops     mem    conns
//	micro  10·2/1  200/300   12.5·8/1  300/200   → fails iops  (67%)
//	small   10%     33%       50%      75%       → fails conns (75% ≥ 70)
//	medium  10%     17%       25%      38%       → fits  → db.t3.medium, $50/mo
func TestDBAnalyzeGolden(t *testing.T) {
	res := DBAnalyzeCfg([]DBInstance{goldenDB("payments-prod", "db.t3.large",
		10, 12.5, 200, 300)}, DefaultConfig())

	if len(res.Skipped) != 0 || len(res.Kept) != 0 {
		t.Fatalf("want no skips/keeps, got skipped=%+v kept=%+v", res.Skipped, res.Kept)
	}
	if len(res.Recommendations) != 1 {
		t.Fatalf("want 1 recommendation, got %d: %+v", len(res.Recommendations), res.Recommendations)
	}
	r := res.Recommendations[0]
	if r.ClassCurrent != "db.t3.large" || r.ClassProposed != "db.t3.medium" {
		t.Fatalf("want db.t3.large → db.t3.medium, got %s → %s", r.ClassCurrent, r.ClassProposed)
	}
	if r.SavingsMonth != 50 {
		t.Fatalf("want $50/mo savings (100 − 50), got %v", r.SavingsMonth)
	}
	if r.Confidence != 1.0 {
		t.Fatalf("want full confidence from 14 days, got %v", r.Confidence)
	}
}

// TestDBAnalyzeSaturatedCurrent: the current class itself cannot keep
// its headroom promises (cpu 90% ≥ 60%) — no candidate can, and the
// bottleneck attribution names cpu (ADR-030 §8).
func TestDBAnalyzeSaturatedCurrent(t *testing.T) {
	res := DBAnalyzeCfg([]DBInstance{goldenDB("bursty", "db.t3.large",
		90, 10, 100, 100)}, DefaultConfig())

	if len(res.Recommendations) != 0 || len(res.Kept) != 1 {
		t.Fatalf("want 0 recs + 1 keep, got recs=%+v kept=%+v", res.Recommendations, res.Kept)
	}
	k := res.Kept[0]
	if k.ClassCurrent != "db.t3.large" || k.Bottleneck != "cpu" {
		t.Fatalf("want keep on db.t3.large with cpu bottleneck, got %+v", k)
	}
}

// TestDBAnalyzeBottleneckFromCheapestCandidate: the current class
// passes every cap, but every cheaper class fails — the keep-rationale
// attributes the cheapest candidate's first failure. t3.xlarge
// (16 GiB / 4800 IOPS / 3200 conns) at cpu 10%, mem 4%, 150 IOPS,
// 1200 connections: every cheaper class fails conns (micro 600%, small
// 300%, medium 150%, large 75% ≥ 70) while cpu/iops/mem all pass.
func TestDBAnalyzeBottleneckFromCheapestCandidate(t *testing.T) {
	res := DBAnalyzeCfg([]DBInstance{goldenDB("conns-heavy", "db.t3.xlarge",
		10, 4, 150, 1200)}, DefaultConfig())

	if len(res.Recommendations) != 0 || len(res.Kept) != 1 {
		t.Fatalf("want 0 recs + 1 keep, got recs=%+v kept=%+v", res.Recommendations, res.Kept)
	}
	if k := res.Kept[0]; k.Bottleneck != "conns" {
		t.Fatalf("want conns bottleneck, got %+v", k)
	}
}

// TestDBAnalyzeCapBoundary pins the cap semantics: a projection exactly
// at the cap (conns 560 of medium's 800 = 70.0%) does not fit — headroom
// promises are strict ("below", ADR-030 §9).
func TestDBAnalyzeCapBoundary(t *testing.T) {
	res := DBAnalyzeCfg([]DBInstance{goldenDB("edge", "db.t3.large",
		5, 6, 100, 560)}, DefaultConfig())

	if len(res.Recommendations) != 0 || len(res.Kept) != 1 || res.Kept[0].Bottleneck != "conns" {
		t.Fatalf("want keep with conns bottleneck at exact cap, got recs=%+v kept=%+v", res.Recommendations, res.Kept)
	}
}

// TestDBAnalyzeSkips covers the eligibility gate: an unknown class and
// insufficient data are skipped, and the data-minimum knob (ADR-024)
// applies to DB instances too.
func TestDBAnalyzeSkips(t *testing.T) {
	unknown := DBInstance{Name: "legacy", Namespace: "db", Class: "db.custom.giant"}
	thin := goldenDB("thin", "db.t3.medium", 10, 10, 100, 100)
	thin.Buckets = thin.Buckets[:3] // 3 days < 5

	res := DBAnalyzeCfg([]DBInstance{unknown, thin}, DefaultConfig())
	if len(res.Recommendations) != 0 || len(res.Skipped) != 2 {
		t.Fatalf("want 2 skips, got recs=%+v skipped=%+v", res.Recommendations, res.Skipped)
	}
	if res.Skipped[0].Reason != "unknown class db.custom.giant" {
		t.Fatalf("unknown-class skip reason: %+v", res.Skipped[0])
	}
	if res.Skipped[1].Reason != "insufficient data" {
		t.Fatalf("insufficient-data skip reason: %+v", res.Skipped[1])
	}

	// Same thin instance sizes once the minimum is relaxed (0.1 = any data).
	// Cheapest-first: micro fits (cpu 20, iops 33, mem 40, conns 50).
	cfg := DefaultConfig()
	cfg.MinDataDays = 0.1
	res = DBAnalyzeCfg([]DBInstance{thin}, cfg)
	if len(res.Recommendations) != 1 || res.Recommendations[0].ClassProposed != "db.t3.micro" {
		t.Fatalf("want thin instance sized under relaxed minimum, got %+v", res)
	}
}

// TestGCPTierCatalogGolden pins the shipped Cloud SQL tier catalog: the
// shared-core tiers plus the db-custom-* tiers Cloud SQL PostgreSQL
// accepts (legacy n1/highmem tiers are not accepted there and stay out),
// price-ordered, with exact capacities and documented default rates.
// It also pins the provider-scoped lookup/step helpers: RDS indices and
// step adjacency are untouched, GCP classes resolve in both lookup
// helpers, and step adjacency works within the GCP tiers.
func TestGCPTierCatalogGolden(t *testing.T) {
	want := []DBClass{
		{Name: "db-f1-micro", VCPU: 0.6, MemGiB: 0.6, MaxIOPS: 3000, MaxConns: 250, PriceUSD: 11},
		{Name: "db-g1-small", VCPU: 1, MemGiB: 1.7, MaxIOPS: 3000, MaxConns: 250, PriceUSD: 43},
		{Name: "db-custom-1-3840", VCPU: 1, MemGiB: 3.75, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 72},
		{Name: "db-custom-2-7680", VCPU: 2, MemGiB: 7.5, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 144},
		{Name: "db-custom-4-15360", VCPU: 4, MemGiB: 15, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 288},
		{Name: "db-custom-8-30720", VCPU: 8, MemGiB: 30, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 575},
	}
	if len(GCPDBCatalog) != len(want) {
		t.Fatalf("GCPDBCatalog: want %d tiers, got %d", len(want), len(GCPDBCatalog))
	}
	for i, c := range GCPDBCatalog {
		if c != want[i] {
			t.Fatalf("GCP tier %d: want %+v, got %+v", i, want[i], c)
		}
		if i > 0 && c.PriceUSD <= GCPDBCatalog[i-1].PriceUSD {
			t.Fatalf("GCP catalog must be price-ordered: %s after %s", c.Name, GCPDBCatalog[i-1].Name)
		}
	}

	// Lookup across both catalogs: db-custom-1-3840 → 1 vCPU / 3.75 GiB / $72.
	c, ok := DBClassByName("db-custom-1-3840")
	if !ok || c.VCPU != 1 || c.MemGiB != 3.75 || c.PriceUSD != 72 {
		t.Fatalf("db-custom-1-3840 lookup: %+v ok=%v", c, ok)
	}
	if DBClassIndex("db-f1-micro") != len(DBCatalog) {
		t.Fatalf("GCP indices must follow RDS: %d", DBClassIndex("db-f1-micro"))
	}
	if DBClassIndex("db-custom-1-3840") != len(DBCatalog)+2 { // f1-micro, g1-small before it
		t.Fatalf("GCP index offset: %d", DBClassIndex("db-custom-1-3840"))
	}
	if DBClassIndex("db.t3.medium") != 2 {
		t.Fatalf("RDS indices must be untouched: %d", DBClassIndex("db.t3.medium"))
	}

	// Step adjacency inside the GCP tiers.
	adj, ok := DBClassStep("db-custom-2-7680")
	if !ok || adj.Name != "db-custom-1-3840" {
		t.Fatalf("GCP step from db-custom-2-7680: %+v ok=%v", adj, ok)
	}
	if _, ok := DBClassStep("db-f1-micro"); ok {
		t.Fatalf("cheapest GCP tier must have no cheaper step")
	}
	// RDS step adjacency unchanged.
	adj, ok = DBClassStep("db.t3.xlarge")
	if !ok || adj.Name != "db.t3.large" {
		t.Fatalf("RDS step must be unchanged: %+v ok=%v", adj, ok)
	}
}

// TestDBAnalyzeGCPGolden is the GCP worked example: db-custom-2-7680
// (2 vCPU / 7.5 GiB / $144) measured at cpu 10%, mem 12.5% (0.94 GiB of
// 7.5), 100 IOPS, 100 connections.
//
//	          cpu       iops       mem        conns
//	f1-micro  10·2/0.6  100/3000   12.5·7.5/0.6   100/250  → fails mem (156%)
//	g1-small  20%       3.3%       55.1%          40%      → fits → db-g1-small, $101/mo
func TestDBAnalyzeGCPGolden(t *testing.T) {
	res := DBAnalyzeCfg([]DBInstance{goldenDB("payments-gcp", "db-custom-2-7680",
		10, 12.5, 100, 100)}, DefaultConfig())

	if len(res.Skipped) != 0 || len(res.Kept) != 0 {
		t.Fatalf("want no skips/keeps, got skipped=%+v kept=%+v", res.Skipped, res.Kept)
	}
	if len(res.Recommendations) != 1 {
		t.Fatalf("want 1 recommendation, got %d: %+v", len(res.Recommendations), res.Recommendations)
	}
	r := res.Recommendations[0]
	if r.ClassCurrent != "db-custom-2-7680" || r.ClassProposed != "db-g1-small" {
		t.Fatalf("want db-custom-2-7680 → db-g1-small, got %s → %s", r.ClassCurrent, r.ClassProposed)
	}
	if r.SavingsMonth != 101 { // 144 − 43
		t.Fatalf("want $101/mo savings, got %v", r.SavingsMonth)
	}
}

// TestDBAnalyzeGCPSearchScopedToProvider: the headroom search never
// crosses providers. db-custom-1-3840 (1 vCPU / 3.75 GiB / $72) at cpu
// 30%, mem 30%, 50 IOPS, 50 connections: g1-small fits at $43 — but so
// would RDS db.t3.small ($25) if RDS candidates were in scope. The
// recommendation must stay inside the GCP catalog.
func TestDBAnalyzeGCPSearchScopedToProvider(t *testing.T) {
	res := DBAnalyzeCfg([]DBInstance{goldenDB("payments-gcp", "db-custom-1-3840",
		30, 30, 50, 50)}, DefaultConfig())

	if len(res.Recommendations) != 1 {
		t.Fatalf("want 1 recommendation, got %d: %+v", len(res.Recommendations), res.Recommendations)
	}
	if r := res.Recommendations[0]; r.ClassProposed != "db-g1-small" {
		t.Fatalf("GCP search must stay in the GCP catalog (db.t3.small would fit but is RDS): %s → %s",
			r.ClassCurrent, r.ClassProposed)
	}
}
