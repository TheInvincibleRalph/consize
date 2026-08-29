// Command verify is the one-shot verifier binary (docs/architecture.md
// §3.8): it finds applied events whose verification window is due and
// has no verification run, compares SLIs against the pre-apply baseline,
// records the verdict, and rolls back on FAIL. Runs as a CronJob.
package main

import (
	"context"
	"log"
	"time"

	"consize/internal/apply"
	"consize/internal/collector"
	"consize/internal/config"
	"consize/internal/dbapply"
	"consize/internal/store"
	"consize/internal/verifier"
)

func main() {
	ctx := context.Background()

	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	if pg, ok := st.(*store.Postgres); ok {
		defer pg.Close()
	}

	patcher, err := apply.NewK8sPatcher(config.Str("KUBECONFIG", ""))
	if err != nil {
		log.Fatalf("kube config: %v (the verifier needs a write identity to roll back)", err)
	}
	applier := apply.NewService(st, patcher, apply.DefaultConfig())
	// Database class events roll back through the DB apply engine. The
	// provider seam is a stub until a live cloud integration lands
	// (ADR-030) — a FAIL verdict then escalates to manual intervention.
	dbApplier := dbapply.NewService(st, dbapply.StubChanger{}, dbapply.DefaultConfig())

	cfg := verifier.DefaultConfig()
	if w := config.Str("CONSIZE_VERIFY_WINDOW", ""); w != "" {
		d, err := time.ParseDuration(w)
		if err != nil {
			log.Fatalf("CONSIZE_VERIFY_WINDOW: %v", err)
		}
		cfg.Window = d
	}
	if m := config.Int("CONSIZE_SUSTAINED_MINUTES", 0); m > 0 {
		cfg.SustainedMinutes = m
	}
	cfg.ErrorExpr = config.Str("CONSIZE_SLI_ERROR_EXPR", "")
	cfg.P99Expr = config.Str("CONSIZE_SLI_P99_EXPR", "")

	prom := collector.NewHTTPPrometheus(config.Str("PROMETHEUS_URL", "http://localhost:9090"), nil)
	// One write surface per kind, dispatched at the same point as the
	// verification itself: class events → DB engine, everything else →
	// k8s engine.
	rollback := func(ctx context.Context, e store.ApplyEvent) error {
		if e.Diff.Resource == store.ResourceClass {
			return dbApplier.Rollback(ctx, e)
		}
		return applier.Rollback(ctx, e)
	}
	v := verifier.New(prom, st, rollback, cfg)

	events, err := st.AppliedEventsUnverified(ctx)
	if err != nil {
		log.Fatalf("list unverified applies: %v", err)
	}
	now := time.Now().UTC()
	due, errorsSeen := 0, 0
	for _, e := range events {
		readyAt := e.CreatedAt.UTC().Add(cfg.Window)
		if !readyAt.Before(now) {
			log.Printf("apply %d: verification window not due (ready at %s)", e.ID, readyAt.Format(time.RFC3339))
			continue
		}
		due++
		verdict, err := v.Verify(ctx, e)
		if err != nil {
			// Transient (Prometheus down, store error): the run wasn't
			// recorded, so the next CronJob tick retries it.
			errorsSeen++
			log.Printf("apply %d: verify failed (retried next tick): %v", e.ID, err)
			continue
		}
		log.Printf("apply %d: verdict %s", e.ID, verdict.String())
	}
	log.Printf("verify done: %d due, %d not due, %d transient errors",
		due, len(events)-due, errorsSeen)
}
