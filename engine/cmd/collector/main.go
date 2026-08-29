// Command collector ingests a window of cluster telemetry into the
// store: workload metadata from the k8s API, usage buckets from
// Prometheus. One-shot by default; with CONSIZE_INTERVAL it loops —
// deployments typically schedule it as a CronJob on a 15-minute
// cadence instead.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"consize/internal/collector"
	"consize/internal/config"
	"consize/internal/dbmetrics"
	"consize/internal/dbmetrics/cloudmonitoring"
	"consize/internal/dbmetrics/cloudwatch"
	"consize/internal/store"
)

func main() {
	kubeconfig := flag.String("kubeconfig", config.Str("CONSIZE_KUBECONFIG", ""),
		"path to kubeconfig; empty = in-cluster")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// CONSIZE_NAMESPACES ("ns1,ns2"): scope workload listing to the named
	// namespaces (ADR-025) so the collector can run with per-namespace
	// read Roles. Empty (the default) lists the whole cluster, which
	// requires a cluster-scope read Role.
	var namespaces []string
	for _, n := range strings.Split(config.Str("CONSIZE_NAMESPACES", ""), ",") {
		if n = strings.TrimSpace(n); n != "" {
			namespaces = append(namespaces, n)
		}
	}

	meta, err := collector.NewK8sMetadata(*kubeconfig, namespaces)
	if err != nil {
		log.Fatalf("k8s metadata: %v", err)
	}

	prom := collector.NewHTTPPrometheus(
		config.Str("PROMETHEUS_URL", "http://localhost:9090"), nil)

	// CONSIZE_DBMETRICS: the database surface (ADR-030 §8). Shipped
	// sources: "fixture" (deterministic demo instance — tests, the
	// live-cluster demo), "cloudwatch" (live RDS via CloudWatch, M3.5)
	// and "gcp" (live Cloud SQL via Cloud Monitoring). Unset = k8s only.
	dbSrc, via, err := dbSourceFor(config.Str("CONSIZE_DBMETRICS", ""))
	if err != nil {
		log.Fatalf("CONSIZE_DBMETRICS: %v", err)
	}
	if via != "" {
		log.Printf("collector: database surface from %s", via)
	}

	c := collector.New(meta, prom, st,
		config.Duration("CONSIZE_STEP", 15*time.Minute),
		config.Duration("CONSIZE_WINDOW", 14*24*time.Hour))
	c.DB = dbSrc
	c.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))

	interval := config.Duration("CONSIZE_INTERVAL", 0)
	if interval <= 0 {
		if err := c.Run(ctx); err != nil {
			log.Fatalf("collect: %v", err)
		}
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.Run(ctx); err != nil {
			log.Printf("collect: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dbSourceFor maps the CONSIZE_DBMETRICS value to a database source.
// via is empty when no database surface is configured (k8s only) and
// otherwise describes the source for the startup log. Unknown values
// fail fast.
func dbSourceFor(kind string) (src dbmetrics.Source, via string, err error) {
	switch kind {
	case "", "none":
		return nil, "", nil
	case "fixture":
		return dbmetrics.NewFixture(), "fixture (CONSIZE_DBMETRICS=fixture)", nil
	case "cloudwatch":
		return cloudwatch.NewSource(), "AWS CloudWatch/RDS (CONSIZE_DBMETRICS=cloudwatch)", nil
	case "gcp":
		return cloudmonitoring.NewSource(), "Google Cloud Monitoring/Cloud SQL (CONSIZE_DBMETRICS=gcp)", nil
	}
	return nil, "", fmt.Errorf("unknown source %q (shipped: fixture, cloudwatch, gcp)", kind)
}
