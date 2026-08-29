package collector

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"consize/internal/dbmetrics"
	"consize/internal/store"
)

// Queries the collector runs against Prometheus. Rate window is fixed at
// 5m: it matches the 15-minute sampling step and smooths cAdvisor's
// scrape-to-scrape jitter without hiding bursts.
const (
	cpuQuery = `sum by (namespace, pod) (rate(container_cpu_usage_seconds_total{container!="POD",container!=""}[5m]) * 1000)`
	memQuery = `sum by (namespace, pod) (container_memory_working_set_bytes{container!="POD",container!=""})`
)

// Collector ingests one window of cluster telemetry into the store:
//
//  1. workload metadata (deployments)     → store.UpsertWorkload
//  2. pod → deployment owner map          → labels series with deployments
//  3. usage series from Prometheus        → aggregated per deployment
//  4. aggregated windows                  → store.UpsertBucket
//  5. database surface (optional)         → DB instances + db_* buckets
type Collector struct {
	Meta   MetadataClient
	Prom   PrometheusClient
	Store  store.Store
	Step   time.Duration // sampling step; 15m
	Window time.Duration // how far back to collect; 14d

	// DB is the optional database surface (ADR-030 §8): nil = k8s only.
	DB dbmetrics.Source

	// metrics override the default queries (tests).
	cpuQuery, memQuery string
	Log                *slog.Logger
}

// New returns a Collector ready to Run.
func New(meta MetadataClient, prom PrometheusClient, st store.Store, step, window time.Duration) *Collector {
	return &Collector{
		Meta: meta, Prom: prom, Store: st,
		Step: step, Window: window,
		cpuQuery: cpuQuery, memQuery: memQuery,
		Log: slog.Default(),
	}
}

// Run executes one full collect cycle. It is safe to call repeatedly:
// upserts are idempotent, so re-collecting a window is cheap.
func (c *Collector) Run(ctx context.Context) error {
	// 1. Workload metadata.
	deployments, err := c.Meta.ListDeployments(ctx)
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}
	workloadIDs := make(map[string]int64, len(deployments)) // "ns/name" → id
	for _, d := range deployments {
		id, err := c.Store.UpsertWorkload(ctx, store.Workload{
			Name:            d.Name,
			Namespace:       d.Namespace,
			Kind:            "deployment",
			Labels:          d.Labels,
			RequestCPUMilli: d.RequestCPUMilli,
			LimitCPUMilli:   d.LimitCPUMilli,
			RequestMemBytes: d.RequestMemBytes,
			LimitMemBytes:   d.LimitMemBytes,
			Source:          "k8s",
		})
		if err != nil {
			return fmt.Errorf("upsert workload %s/%s: %w", d.Namespace, d.Name, err)
		}
		workloadIDs[d.Namespace+"/"+d.Name] = id
	}

	// 2. Pod → deployment ownership (drops pods of jobs, daemonsets, …).
	owners, err := c.Meta.PodOwners(ctx)
	if err != nil {
		return fmt.Errorf("resolve pod owners: %w", err)
	}

	// 3+4. Usage series → per-deployment buckets.
	end := time.Now().UTC()
	start := end.Add(-c.Window)
	for _, job := range []struct {
		query  string
		metric string
	}{
		{c.cpuQuery, store.MetricCPUMilli},
		{c.memQuery, store.MetricMemBytes},
	} {
		if err := c.collectMetric(ctx, job.query, job.metric, start, end, workloadIDs, owners); err != nil {
			return err
		}
	}
	// 5. Database surface (ADR-030 §8), when configured.
	if c.DB != nil {
		if err := c.ingestDB(ctx, start, end); err != nil {
			return err
		}
	}
	return nil
}

// ingestDB ingests the database surface: instances upsert as Workloads
// (Source="db") and their series land in usage_buckets with the db_*
// metric names. The k8s Prometheus client is not consulted — DB sources
// produce their own series (CloudWatch/Cloud Monitoring later).
func (c *Collector) ingestDB(ctx context.Context, start, end time.Time) error {
	insts, err := c.DB.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list db instances: %w", err)
	}
	for _, inst := range insts {
		id, err := c.Store.UpsertWorkload(ctx, store.Workload{
			Name: inst.Name, Namespace: inst.Namespace, Kind: "database", Source: "db",
			Labels: inst.Labels, DBClass: inst.Class, DBReplicas: inst.Replicas,
			DBMaintenanceWindow: inst.MaintenanceWindow, DBProvider: inst.Provider,
		})
		if err != nil {
			return fmt.Errorf("upsert db instance %s: %w", inst.Name, err)
		}
		buckets := 0
		for _, metric := range []string{
			store.MetricDBCPUPercent, store.MetricDBIOPS,
			store.MetricDBConnections, store.MetricDBMemPercent, store.MetricDBErrors,
		} {
			series, err := c.DB.Series(ctx, inst, metric, start, end, c.Step)
			if err != nil {
				return fmt.Errorf("db series %s %s: %w", inst.Name, metric, err)
			}
			for _, b := range series {
				b.WorkloadID = id
				if err := c.Store.UpsertBucket(ctx, b); err != nil {
					return err
				}
			}
			buckets += len(series)
		}
		c.Log.Info("collector: db instance ingested", "instance", inst.Name,
			"class", inst.Class, "buckets", buckets)
	}
	return nil
}

// collectMetric aggregates one Prometheus query into usage buckets.
//
// Series arrive per pod; every point within a window for pods of the same
// deployment is summed — the deployment's usage is the sum of its pods —
// and stored as a single-sample window (P50=P95=P99=Max=sum).
func (c *Collector) collectMetric(ctx context.Context, query, metric string,
	start, end time.Time, workloadIDs map[string]int64, owners map[string]string) error {

	series, err := c.Prom.QueryRange(ctx, query, start, end, c.Step)
	if err != nil {
		return err
	}

	// workloadID → windowStart → (sum of pod values, pod count)
	agg := map[int64]map[int64]aggPoint{}
	var unknown, stored int
	for _, s := range series {
		ns, pod := s.Metric["namespace"], s.Metric["pod"]
		dep, ok := owners[ns+"/"+pod]
		if !ok {
			unknown++
			continue // not owned by a deployment we manage
		}
		wid, ok := workloadIDs[ns+"/"+dep]
		if !ok {
			unknown++
			continue
		}
		byWindow := agg[wid]
		if byWindow == nil {
			byWindow = map[int64]aggPoint{}
			agg[wid] = byWindow
		}
		for _, p := range s.Points {
			ts := p.Timestamp.Unix() - (p.Timestamp.Unix() % int64(c.Step.Seconds())) // align to step grid
			v := byWindow[ts]
			v.sum += p.Value
			v.pods++
			byWindow[ts] = v
		}
	}

	for wid, byWindow := range agg {
		for ts, v := range byWindow {
			b := store.Bucket{
				WorkloadID:  wid,
				Metric:      metric,
				WindowStart: time.Unix(ts, 0).UTC(),
				P50:         v.sum, P95: v.sum, P99: v.sum, Max: v.sum,
				Samples: v.pods,
			}
			if err := c.Store.UpsertBucket(ctx, b); err != nil {
				return err
			}
			stored++
		}
	}
	if unknown > 0 {
		c.Log.Info("collector: series without a managed deployment owner",
			"metric", metric, "series", unknown)
	}
	c.Log.Info("collector: buckets upserted", "metric", metric, "buckets", stored)
	return nil
}

type aggPoint struct {
	sum  float64
	pods int
}
