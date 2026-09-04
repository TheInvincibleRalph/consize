// Package fixtures ships the synthetic workloads Consize is tested and
// demoed against — the same scenario as docs/demo.md and docs/testing.md.
//
// The set is deterministic (fixed seed), so every run — and every CI run —
// produces the same report and the same test assertions.
package fixtures

import (
	"math/rand"

	"consize/internal/analysis"
)

const (
	baseEpoch     = 1750032000 // day-aligned epoch (20255 days since Unix epoch): every day holds exactly bucketsPerDay samples
	bucketsPerDay = 96         // 15-minute buckets
	demoDays      = 14
	excludeLabel  = "consize.savings.dev/exclude"
	riskLabel     = "consize.savings.dev/data-loss-risk"
)

// Workloads returns the shipped demo set: 10 workloads covering the full
// policy surface — inflated requests, bursty usage, exclusions, protected
// namespaces, stateful risks, insufficient data, and already-optimal.
func Workloads() []analysis.Workload {
	rng := rand.New(rand.NewSource(42)) // deterministic

	ws := []analysis.Workload{
		// The demo star: 8 GiB requested, ~300 MiB used.
		steady("checkout-api", "apps", "deployment", 8192, 16384, 2000, 4000, 300, 20, 120, 10, rng, -1),
		// Healthy but oversized.
		steady("payments-service", "apps", "deployment", 4096, 8192, 1000, 2000, 1200, 30, 300, 15, rng, -1),
		// Bursty: quiet most days, one spike day.
		steady("inventory-service", "apps", "deployment", 2048, 4096, 1000, 2000, 400, 20, 250, 15, rng, 7),
		// Idle worker with a big static request.
		steady("notifications-worker", "jobs", "deployment", 6144, 8192, 1000, 2000, 400, 15, 50, 5, rng, -1),
		// Already optimal: requests == usage on both resources,
		// downsize-only policy skips it.
		steady("legacy-reporting", "analytics", "deployment", 2048, 4096, 500, 1000, 2048, 0, 500, 0, rng, -1),
		// Mid-sized recommendation.
		steady("image-processor", "media", "deployment", 4096, 8192, 2000, 4000, 2500, 50, 800, 30, rng, -1),
		// Repo-managed Online Boutique example for testing the Kubernetes YAML
		// PR workflow against the devops-portfolio repository.
		withLabel(steady("redis-cart", "boutique", "deployment", 140, 179, 70, 125, 8, 2, 15, 3, rng, -1), "consize.dev/container", "redis"),
		// Vendored Online Boutique example with no local overlay. This proves
		// Consize can patch the repo-owned upstream manifest when an overlay
		// patch does not exist yet.
		withLabel(steady("recommendationservice", "boutique", "deployment", 220, 450, 100, 200, 45, 4, 28, 5, rng, -1), "consize.dev/container", "server"),

		// Skipped by policy:
		withLabel(steady("crypto-miner-ha", "apps", "deployment", 8192, 16384, 2000, 4000, 100, 10, 40, 5, rng, -1), excludeLabel, "true"),
		steady("calico-node", "kube-system", "daemonset", 1024, 2048, 500, 1000, 600, 20, 200, 10, rng, -1),
		withLabel(steady("analytics-db-syncer", "data", "statefulset", 8192, 16384, 2000, 4000, 512, 20, 100, 10, rng, -1), riskLabel, "true"),
	}

	// beta-service: only 3 days of data — skipped for insufficiency.
	beta := steady("beta-service", "apps", "deployment", 4096, 8192, 1000, 2000, 2000, 30, 500, 20, rng, -1)
	beta.Buckets = beta.Buckets[:3*bucketsPerDay]
	ws = append(ws, beta)

	return ws
}

// withLabel stamps a label onto a workload copy.
func withLabel(w analysis.Workload, key, value string) analysis.Workload {
	if w.Labels == nil {
		w.Labels = map[string]string{}
	}
	w.Labels[key] = value
	return w
}

// steady builds a workload with demoDays × bucketsPerDay deterministic
// samples: base usage ± noise, with an optional spike on spikeDay (-1 = none).
func steady(name, ns, kind string,
	reqMemMiB, limMemMiB, reqCPU, limCPU int64,
	memBase, memNoise, cpuBase, cpuNoise float64,
	rng *rand.Rand, spikeDay int,
) analysis.Workload {
	w := analysis.Workload{
		Name:       name,
		Namespace:  ns,
		Kind:       kind,
		RequestMem: reqMemMiB << 20,
		LimitMem:   limMemMiB << 20,
		RequestCPU: reqCPU,
		LimitCPU:   limCPU,
	}

	for d := 0; d < demoDays; d++ {
		for b := 0; b < bucketsPerDay; b++ {
			m := memBase + (rng.Float64()*2-1)*memNoise
			c := cpuBase + (rng.Float64()*2-1)*cpuNoise
			if d == spikeDay {
				m *= 8
				c *= 6
			}
			w.Buckets = append(w.Buckets, analysis.Bucket{
				MemUsedBytes: int64(m) << 20,
				CPUUsedMilli: int64(c),
				WindowStart:  baseEpoch + int64(d*86400+b*900),
			})
		}
	}
	return w
}
