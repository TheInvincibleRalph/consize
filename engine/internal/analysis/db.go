// DB sizing policy (ADR-030): database instances are workloads, and
// their sizing is a headroom-constrained candidate search over an
// instance-class catalog. Like the k8s policy, everything here is pure —
// no I/O — so the whole policy is golden-tested.
//
// Projection model (§6): CPU and memory arrive as percents of the
// *current* class's capacity and scale inversely with the candidate's
// capacity ratio; IOPS and connections arrive as absolute counts, so a
// candidate's utilization is measured ÷ its catalog baseline. A
// candidate fits when every projected p95 is below its cap (§9):
//
//	cpu   < 60%   (CPU headroom)
//	iops  < 60%   (IOPS headroom above 40%)
//	mem   < 75%   (free memory above 25%)
//	conns < 70%   (connection headroom)
//
// The search is downsize-only (never a class more expensive than the
// current one) and picks the cheapest class that fits — maximum savings
// that keeps every headroom promise. When nothing fits, the result is a
// "keep with rationale": the bottleneck dimension is attributed in
// policy order (cpu, iops, mem, conns) — the current class itself when
// it is saturated, otherwise the cheapest candidate's first failure.
package analysis

import (
	"math"
	"sort"
)

// DB metric names, mirroring the store's usage_buckets names (ADR-030).
// Kept local to the policy so analysis stays dependency-free.
const (
	DBMetricCPUPercent  = "db_cpu_percent"
	DBMetricIOPS        = "db_iops"
	DBMetricConnections = "db_connections"
	DBMetricMemPercent  = "db_mem_percent"
)

// Headroom caps on projected p95 utilization (ADR-030 §9). Shipped
// constants like the k8s policy's Headroom/MinLimitMult — not knobs.
const (
	DBCPUCap   = 60.0 // projected CPU % must be below this
	DBIOPSCap  = 60.0 // projected IOPS % must be below this
	DBMemCap   = 75.0 // projected memory % must be below this
	DBConnsCap = 70.0 // projected connection % must be below this
)

// DBClass is one catalog entry: the instance class's capacity baselines
// and its on-demand monthly price. Each catalog is ordered cheapest →
// most expensive; catalog[i].PriceUSD < catalog[i+1].PriceUSD.
type DBClass struct {
	Name     string
	VCPU     float64 // fractional vCPUs (Cloud SQL shared-core tiers)
	MemGiB   float64 // fractional GiB (Cloud SQL tiers)
	MaxIOPS  int
	MaxConns int
	PriceUSD float64
}

// DBCatalog is the shipped RDS instance-class catalog (ADR-030 §4).
// Prices are on-demand monthly approximations; the real price service
// is a future refinement (same seam as the k8s Prices table).
var DBCatalog = []DBClass{
	{Name: "db.t3.micro", VCPU: 1, MemGiB: 1, MaxIOPS: 300, MaxConns: 200, PriceUSD: 15},
	{Name: "db.t3.small", VCPU: 2, MemGiB: 2, MaxIOPS: 600, MaxConns: 400, PriceUSD: 25},
	{Name: "db.t3.medium", VCPU: 2, MemGiB: 4, MaxIOPS: 1200, MaxConns: 800, PriceUSD: 50},
	{Name: "db.t3.large", VCPU: 2, MemGiB: 8, MaxIOPS: 2400, MaxConns: 1600, PriceUSD: 100},
	{Name: "db.t3.xlarge", VCPU: 4, MemGiB: 16, MaxIOPS: 4800, MaxConns: 3200, PriceUSD: 200},
}

// GCPDBCatalog is the shipped Cloud SQL tier catalog: shared-core
// (db-f1-micro, db-g1-small) plus the db-custom-* tiers Cloud SQL
// PostgreSQL accepts (legacy n1/highmem tiers are not accepted there,
// so they stay out of the catalog). vCPUs and GiB are fractional for
// the shared-core tiers; prices are on-demand monthly approximations
// (us-central1 public pricing, documented as defaults) — the real
// price service is a future refinement. Cloud SQL publishes no per-tier
// IOPS or connection caps the way RDS does (IOPS scale with storage,
// max_connections is a tunable flag defaulting to 1000, 250 on
// shared-core), so MaxIOPS/MaxConns carry those documented defaults as
// baselines and rarely discriminate between tiers — which reflects how
// the service behaves.
var GCPDBCatalog = []DBClass{
	{Name: "db-f1-micro", VCPU: 0.6, MemGiB: 0.6, MaxIOPS: 3000, MaxConns: 250, PriceUSD: 11},
	{Name: "db-g1-small", VCPU: 1, MemGiB: 1.7, MaxIOPS: 3000, MaxConns: 250, PriceUSD: 43},
	{Name: "db-custom-1-3840", VCPU: 1, MemGiB: 3.75, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 72},
	{Name: "db-custom-2-7680", VCPU: 2, MemGiB: 7.5, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 144},
	{Name: "db-custom-4-15360", VCPU: 4, MemGiB: 15, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 288},
	{Name: "db-custom-8-30720", VCPU: 8, MemGiB: 30, MaxIOPS: 48000, MaxConns: 1000, PriceUSD: 575},
}

// DBInstance is one database's measured usage over the analysis window.
// Buckets carry the per-metric p95 samples (store names in
// DBMetric*); the policy derives per-day p95s and the confidence gate
// from them, exactly like the k8s side.
type DBInstance struct {
	Name      string
	Namespace string
	Class     string // current instance class, e.g. db.t3.large
	Buckets   []DBBucket
}

// DBBucket is one 15-minute p95 sample of one DB metric.
type DBBucket struct {
	Metric      string
	WindowStart int64 // unix seconds; day = WindowStart / 86400
	Value       float64
}

// DBRecommendation is one instance's class change.
type DBRecommendation struct {
	Workload      string
	Namespace     string
	ClassCurrent  string
	ClassProposed string
	SavingsMonth  float64
	Confidence    float64 // 0..1, based on data volume
}

// DBKeep is the "keep with rationale" outcome (plan M3): the instance
// stays on its current class because no cheaper class can keep its
// headroom promises. Bottleneck names the failing dimension in policy
// order (cpu | iops | mem | conns).
type DBKeep struct {
	Workload     string
	Namespace    string
	ClassCurrent string
	Bottleneck   string
}

// DBResult bundles everything a run produced.
type DBResult struct {
	Recommendations []DBRecommendation
	Kept            []DBKeep
	Skipped         []Skipped
}

// DBAnalyze runs the DB sizing policy with the shipped defaults. See
// DBAnalyzeCfg for the configurable variant.
func DBAnalyze(instances []DBInstance) DBResult {
	return DBAnalyzeCfg(instances, DefaultConfig())
}

// DBAnalyzeCfg runs the DB sizing policy over an instance set: skip
// conditions first (unknown class, insufficient data), then candidate
// search, then recommendations sorted by savings.
func DBAnalyzeCfg(instances []DBInstance, cfg Config) DBResult {
	var res DBResult
	for _, in := range instances {
		cur, ok := dbClass(in.Class)
		if !ok {
			res.Skipped = append(res.Skipped, Skipped{Workload: in.Name, Namespace: in.Namespace, Reason: "unknown class " + in.Class})
			continue
		}
		d := dbDaysOfData(in.Buckets)
		if float64(d) < cfg.MinDataDays {
			res.Skipped = append(res.Skipped, Skipped{Workload: in.Name, Namespace: in.Namespace, Reason: "insufficient data"})
			continue
		}
		obs := dbObservation(in.Buckets)
		if r, bottleneck, fits := dbSizing(cur, obs); fits {
			r.Workload = in.Name
			r.Namespace = in.Namespace
			r.Confidence = dbConfidence(d)
			res.Recommendations = append(res.Recommendations, r)
		} else {
			res.Kept = append(res.Kept, DBKeep{
				Workload:     in.Name,
				Namespace:    in.Namespace,
				ClassCurrent: cur.Name,
				Bottleneck:   bottleneck,
			})
		}
	}
	sort.Slice(res.Recommendations, func(i, j int) bool {
		return res.Recommendations[i].SavingsMonth > res.Recommendations[j].SavingsMonth
	})
	return res
}

// DBClassByName looks up a catalog entry by name across both provider
// catalogs (RDS first, then GCP; RDS names keep their DBCatalog
// identity). Exported for the CloudWatch adapter, which needs class
// memory to turn FreeableMemory bytes into a mem percent, and for the
// API's recommendation risk flags.
func DBClassByName(name string) (DBClass, bool) {
	for _, c := range DBCatalog {
		if c.Name == name {
			return c, true
		}
	}
	for _, c := range GCPDBCatalog {
		if c.Name == name {
			return c, true
		}
	}
	return DBClass{}, false
}

// DBClassIndex returns the catalog index of a class across both
// provider catalogs: RDS classes keep their DBCatalog indices (0 =
// cheapest), GCP classes are offset by len(DBCatalog). Indices of two
// same-provider classes differ by their relative price steps, which is
// all the callers (risk flags, stepped applies) rely on. Returns -1
// for an unknown class.
func DBClassIndex(name string) int {
	for i, c := range DBCatalog {
		if c.Name == name {
			return i
		}
	}
	for i, c := range GCPDBCatalog {
		if c.Name == name {
			return len(DBCatalog) + i
		}
	}
	return -1
}

// DBClassStep returns the next-cheaper class in the same provider
// catalog as name — one adjacent step toward a downsize target (the
// stepped-apply path). ok=false when name is unknown or already the
// cheapest class of its catalog (no cheaper step exists).
func DBClassStep(name string) (DBClass, bool) {
	for i, c := range dbCatalogOf(name) {
		if c.Name == name {
			if i == 0 {
				return DBClass{}, false
			}
			return dbCatalogOf(name)[i-1], true
		}
	}
	return DBClass{}, false
}

// dbCatalogOf returns the provider catalog containing name, or nil.
// The headroom search and step adjacency are provider-scoped: a class
// proposal must stay in the instance's provider (an RDS instance never
// steps to a Cloud SQL tier, and vice versa).
func dbCatalogOf(name string) []DBClass {
	for _, c := range DBCatalog {
		if c.Name == name {
			return DBCatalog
		}
	}
	for _, c := range GCPDBCatalog {
		if c.Name == name {
			return GCPDBCatalog
		}
	}
	return nil
}

// dbClass looks up a catalog entry by name.
func dbClass(name string) (DBClass, bool) {
	return DBClassByName(name)
}

// dbObs is the window's measured utilization per dimension: the p95 of
// the per-day p95s, mirroring the k8s dailyP95→Percentile path.
type dbObs struct {
	CPUPercent float64
	MemPercent float64
	IOPS       float64
	Conns      float64
}

func dbObservation(buckets []DBBucket) dbObs {
	return dbObs{
		CPUPercent: Percentile(dbDailyP95(buckets, DBMetricCPUPercent), 95),
		MemPercent: Percentile(dbDailyP95(buckets, DBMetricMemPercent), 95),
		IOPS:       Percentile(dbDailyP95(buckets, DBMetricIOPS), 95),
		Conns:      Percentile(dbDailyP95(buckets, DBMetricConnections), 95),
	}
}

// dbDailyP95 returns each day's p95 of one metric.
func dbDailyP95(buckets []DBBucket, metric string) []float64 {
	byDay := map[int64][]float64{}
	for _, b := range buckets {
		if b.Metric == metric {
			byDay[b.WindowStart/86400] = append(byDay[b.WindowStart/86400], b.Value)
		}
	}
	out := make([]float64, 0, len(byDay))
	for _, vals := range byDay {
		out = append(out, Percentile(vals, 95))
	}
	return out
}

// dbDaysOfData counts distinct days with at least one bucket of any DB
// metric.
func dbDaysOfData(buckets []DBBucket) int {
	seen := map[int64]bool{}
	for _, b := range buckets {
		seen[b.WindowStart/86400] = true
	}
	return len(seen)
}

func dbConfidence(days int) float64 {
	return math.Min(float64(days)/ConfidenceDays, 1.0)
}

// dbProjection is one candidate class's projected p95 utilization, as
// percents of the candidate's capacity.
type dbProjection struct {
	cpu, iops, mem, conns float64
}

// dbProject projects the observed utilization onto a candidate class
// (ADR-030 §6): percents scale inversely with the capacity ratio;
// absolute counts divide by the candidate's catalog baseline.
func dbProject(cur, cand DBClass, obs dbObs) dbProjection {
	return dbProjection{
		cpu:   obs.CPUPercent * cur.VCPU / cand.VCPU,
		iops:  obs.IOPS / float64(cand.MaxIOPS) * 100,
		mem:   obs.MemPercent * cur.MemGiB / cand.MemGiB,
		conns: obs.Conns / float64(cand.MaxConns) * 100,
	}
}

// dbFails reports the first cap exceeded by a projection, in policy
// order (cpu, iops, mem, conns), or ok=false when all caps hold.
func dbFails(p dbProjection) (dim string, fails bool) {
	switch {
	case p.cpu >= DBCPUCap:
		return "cpu", true
	case p.iops >= DBIOPSCap:
		return "iops", true
	case p.mem >= DBMemCap:
		return "mem", true
	case p.conns >= DBConnsCap:
		return "conns", true
	}
	return "", false
}

// dbSizing runs the headroom-constrained candidate search. Returns the
// recommendation, the bottleneck attribution when nothing fits, and
// whether a candidate fits. Downsize-only: candidates are the current
// class's provider catalog (RDS or GCP — the search never crosses
// providers) strictly cheaper than the current one, cheapest first, so
// the first fit is the maximum-savings class that keeps every promise.
func dbSizing(cur DBClass, obs dbObs) (DBRecommendation, string, bool) {
	// The current class itself is the reference: when it cannot keep its
	// own headroom promises, no cheaper class can — bottleneck attribution
	// names the saturated dimension.
	if dim, fails := dbFails(dbProject(cur, cur, obs)); fails {
		return DBRecommendation{}, dim, false
	}
	// Cheapest candidate's first failure is the keep-rationale when
	// nothing smaller fits (the class the search most wanted to try).
	bottleneck := ""
	for _, cand := range dbCatalogOf(cur.Name) {
		if cand.PriceUSD >= cur.PriceUSD {
			continue // downsize-only
		}
		p := dbProject(cur, cand, obs)
		if dim, fails := dbFails(p); fails {
			if bottleneck == "" {
				bottleneck = dim
			}
			continue
		}
		return DBRecommendation{
			ClassCurrent:  cur.Name,
			ClassProposed: cand.Name,
			SavingsMonth:  cur.PriceUSD - cand.PriceUSD,
		}, "", true
	}
	return DBRecommendation{}, bottleneck, false
}
