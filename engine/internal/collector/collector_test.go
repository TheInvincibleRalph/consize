package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"consize/internal/store"
)

// fakeProm holds canned query_range bodies per query and records the
// request URLs for assertions. Served through newPromServer.
type fakeProm struct {
	cpu, mem string // canned JSON bodies
	reqs     []string
}

type fakeMeta struct {
	deployments []DeploymentInfo
	owners      map[string]string
}

func (f fakeMeta) ListDeployments(context.Context) ([]DeploymentInfo, error) {
	return f.deployments, nil
}
func (f fakeMeta) PodOwners(context.Context) (map[string]string, error) { return f.owners, nil }

// newPromServer serves query_range from the fake's canned bodies.
func newPromServer(f *fakeProm) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reqs = append(f.reqs, r.URL.String())
		switch r.URL.Query().Get("query") {
		case cpuQuery:
			fmt.Fprint(w, f.cpu)
		case memQuery:
			fmt.Fprint(w, f.mem)
		default:
			http.Error(w, "unexpected query: "+r.URL.Query().Get("query"), http.StatusBadRequest)
		}
	}))
}

// base is aligned to the 15-minute grid: 1750032000 % 900 == 0.
const base = 1750032000

func matrixSeries(series ...string) string {
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, strings.Join(series, ","))
}

func ser(namespace, pod string, values [][2]any) string {
	b, _ := jsonMarshal(values)
	return fmt.Sprintf(`{"metric":{"namespace":%q,"pod":%q},"values":%s}`, namespace, pod, b)
}

func jsonMarshal(v any) (string, error) { // tiny helper to keep literals readable
	b, err := json.Marshal(v)
	return string(b), err
}

func TestCollectorAggregatesPodsToDeployment(t *testing.T) {
	ctx := context.Background()
	meta := fakeMeta{
		deployments: []DeploymentInfo{{
			Name: "api", Namespace: "prod",
			RequestCPUMilli: 1000, LimitCPUMilli: 2000,
			RequestMemBytes: 1 << 30, LimitMemBytes: 2 << 30,
		}},
		owners: map[string]string{
			"prod/api-abc": "api",
			"prod/api-def": "api",
			// orphan pod deliberately absent
		},
	}
	fake := &fakeProm{
		cpu: matrixSeries(
			ser("prod", "api-abc", [][2]any{{base, "100"}, {base + 900, "200"}}),
			ser("prod", "api-def", [][2]any{{base, "10"}, {base + 900, "20"}}),
			ser("prod", "orphan-1", [][2]any{{base, "999"}}),
		),
		mem: matrixSeries(
			ser("prod", "api-abc", [][2]any{{base, "1024"}, {base + 900, "2048"}}),
			ser("prod", "api-def", [][2]any{{base, "256"}, {base + 900, "512"}}),
		),
	}
	prom := NewHTTPPrometheus(newPromServer(fake).URL, nil)
	st := store.NewMemory()

	c := New(meta, prom, st, 15*time.Minute, 14*24*time.Hour)
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Workload persisted with declared resources.
	wl, err := st.ListWorkloads(ctx)
	if err != nil || len(wl) != 1 {
		t.Fatalf("workloads: %d err=%v", len(wl), err)
	}
	if wl[0].RequestCPUMilli != 1000 || wl[0].RequestMemBytes != 1<<30 {
		t.Fatalf("workload resources wrong: %+v", wl[0])
	}

	// CPU buckets: per-window sums of both pods, orphan dropped.
	cpu, err := st.ListBuckets(ctx, wl[0].ID, store.MetricCPUMilli, time.Time{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(cpu) != 2 {
		t.Fatalf("cpu buckets: %d", len(cpu))
	}
	if cpu[0].WindowStart.Unix() != base || cpu[0].P95 != 110 || cpu[0].Samples != 2 {
		t.Fatalf("cpu bucket 0: %+v", cpu[0])
	}
	if cpu[1].P95 != 220 {
		t.Fatalf("cpu bucket 1: %+v", cpu[1])
	}

	// Memory buckets.
	mem, err := st.ListBuckets(ctx, wl[0].ID, store.MetricMemBytes, time.Time{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(mem) != 2 || mem[0].P95 != 1280 || mem[1].P95 != 2560 {
		t.Fatalf("mem buckets: %+v", mem)
	}

	// Queries must have been the two canonical ones with the right step.
	if len(fake.reqs) != 2 {
		t.Fatalf("prometheus requests: %d", len(fake.reqs))
	}
	for _, q := range fake.reqs {
		if !strings.Contains(q, "step=900s") {
			t.Fatalf("step not 900s: %s", q)
		}
		if !strings.Contains(q, "start=") || !strings.Contains(q, "end=") {
			t.Fatalf("range missing: %s", q)
		}
	}
}

func TestCollectorSkipsSeriesForUnknownDeployment(t *testing.T) {
	ctx := context.Background()
	meta := fakeMeta{
		deployments: []DeploymentInfo{{Name: "api", Namespace: "prod"}},
		// The pod's owner resolves to a deployment the metadata step
		// didn't return (list race, or partial failure) — its series
		// must be dropped, not attributed to a wrong workload.
		owners: map[string]string{"prod/api-1": "ghost"},
	}
	fake := &fakeProm{
		cpu: matrixSeries(ser("prod", "api-1", [][2]any{{base, "50"}})),
		mem: matrixSeries(ser("prod", "api-1", [][2]any{{base, "50"}})),
	}
	st := store.NewMemory()
	c := New(meta, NewHTTPPrometheus(newPromServer(fake).URL, nil), st, 15*time.Minute, time.Hour)
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	wl, err := st.ListWorkloads(ctx)
	if err != nil || len(wl) != 1 {
		t.Fatalf("workloads: %d err=%v", len(wl), err)
	}
	if got, err := st.ListBuckets(ctx, wl[0].ID, store.MetricCPUMilli, time.Time{}, time.Now().Add(time.Hour)); err == nil && len(got) != 0 {
		t.Fatalf("expected no buckets for unknown deployment, got %d", len(got))
	}
}

func TestCollectorPrometheusError(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	st := store.NewMemory()
	c := New(fakeMeta{}, NewHTTPPrometheus(srv.URL, nil), st, 15*time.Minute, time.Hour)
	if err := c.Run(ctx); err == nil {
		t.Fatal("want error from failing prometheus")
	}
}
