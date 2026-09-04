// Package apply provides a DemoPatcher for the interactive demo sandbox.
// It implements the Patcher interface without touching a real Kubernetes
// cluster: patches are recorded in memory, ReadResources returns the
// tracked values, and Health always passes. This lets the full apply
// engine (guardrails, step logic, audit trail, rollback) run end-to-end
// so developers experience the real safety engine.
//
// Wire it with CONSIZE_DEMO_PATCHER=true.
package apply

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"consize/internal/store"
)

// DemoPatcher satisfies the Patcher interface without a live cluster.
// It is intentionally exported so the API cmd can reference it by name.
type DemoPatcher struct {
	mu        sync.Mutex
	resources map[string]resourceState // key: "namespace/name/kind"
}

type resourceState struct {
	req int64
	lim int64
}

// NewDemoPatcher returns a DemoPatcher pre-loaded with the fixture
// workload values so ReadResources returns sensible before/after numbers.
func NewDemoPatcher() *DemoPatcher {
	p := &DemoPatcher{
		resources: make(map[string]resourceState),
	}
	// Pre-seed with the fixture workloads so rollback math is correct.
	seed := []struct {
		ns, name, kind string
		req, lim       int64
	}{
		{"apps", "checkout-api", "cpu", 2000, 4000},
		{"apps", "checkout-api", "memory", 8192 << 20, 16384 << 20},
		{"apps", "payments-service", "cpu", 1000, 2000},
		{"apps", "payments-service", "memory", 4096 << 20, 8192 << 20},
		{"apps", "inventory-service", "cpu", 1000, 2000},
		{"apps", "inventory-service", "memory", 2048 << 20, 4096 << 20},
		{"jobs", "notifications-worker", "cpu", 1000, 2000},
		{"jobs", "notifications-worker", "memory", 6144 << 20, 8192 << 20},
		{"media", "image-processor", "cpu", 2000, 4000},
		{"media", "image-processor", "memory", 4096 << 20, 8192 << 20},
		{"boutique", "redis-cart", "cpu", 70, 125},
		{"boutique", "redis-cart", "memory", 140 << 20, 179 << 20},
		{"boutique", "recommendationservice", "cpu", 100, 200},
		{"boutique", "recommendationservice", "memory", 220 << 20, 450 << 20},
	}
	for _, s := range seed {
		p.resources[key(s.ns, s.name, s.kind)] = resourceState{req: s.req, lim: s.lim}
	}
	return p
}

func key(ns, name, kind string) string {
	return ns + "/" + name + "/" + kind
}

// Health always passes — there is no cluster to ping.
func (d *DemoPatcher) Health(_ context.Context) error { return nil }

// ReadResources returns the in-memory tracked values for the workload.
// Returns the seeded values on first call; subsequent calls reflect any
// patches applied so rollback math lands on the pre-apply value.
func (d *DemoPatcher) ReadResources(_ context.Context, namespace, name, kind string) (req, lim int64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := key(namespace, name, kind)
	if s, ok := d.resources[k]; ok {
		return s.req, s.lim, nil
	}
	// Unknown workload: return plausible defaults.
	return 1000, 2000, nil
}

// PatchDeployment records the new values in memory and prints a realistic
// log line so the demo terminal output looks authentic.
func (d *DemoPatcher) PatchDeployment(_ context.Context, namespace, name string, diff store.Diff) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := key(namespace, name, diff.Resource)
	d.resources[k] = resourceState{req: diff.ProposedReq, lim: diff.ProposedLimit}

	unit := "MiB"
	reqFrom, reqTo := fmt.Sprintf("%d", diff.CurrentReq), fmt.Sprintf("%d", diff.ProposedReq)
	if diff.Resource == "cpu" {
		unit = "m"
	} else {
		// Memory: display in MiB for readability.
		reqFrom = fmt.Sprintf("%d", diff.CurrentReq>>20)
		reqTo = fmt.Sprintf("%d", diff.ProposedReq>>20)
	}

	slog.Info("[demo] deployment patched",
		"namespace", namespace,
		"name", name,
		"resource", diff.Resource,
		"from", reqFrom+unit,
		"to", reqTo+unit,
	)
	return nil
}

