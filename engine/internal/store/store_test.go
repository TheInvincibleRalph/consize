package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"consize/internal/store"
)

// stores returns the implementations under test. The behavior suite runs
// against every entry: memory always, Postgres when CONSIZE_TEST_POSTGRES
// points at a disposable database.
func stores(t *testing.T) map[string]store.Store {
	t.Helper()
	out := map[string]store.Store{"memory": store.NewMemory()}
	if url := os.Getenv("CONSIZE_TEST_POSTGRES"); url != "" {
		ctx := context.Background()
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		if err := store.Migrate(ctx, pool); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		// Fresh tables per run.
		if _, err := pool.Exec(ctx,
			`TRUNCATE app_settings, sessions, users, verification_runs, apply_events, recommendations, usage_buckets, workloads, teams RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		out["postgres"] = store.NewPostgresFromPool(pool)
	}
	return out
}

func TestTeamsOwnWorkloadsAndSurviveCollectorUpserts(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			team, err := st.CreateTeam(ctx, store.Team{
				Slug: "payments", Name: "Payments", Owner: "Ada Lovelace", OnCall: "#payments-oncall",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateTeam(ctx, store.Team{Slug: "payments", Name: "Duplicate", Owner: "A", OnCall: "B"}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate slug: want ErrConflict, got %v", err)
			}

			wid, err := st.UpsertWorkload(ctx, store.Workload{Name: "checkout", Namespace: "prod", Source: "k8s"})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetWorkloadTeam(ctx, wid, &team.ID); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetWorkload(ctx, wid)
			if err != nil {
				t.Fatal(err)
			}
			if got.TeamID != team.ID || got.TeamName != "Payments" || got.TeamOnCall != "#payments-oncall" {
				t.Fatalf("assignment did not round-trip: %+v", got)
			}

			// The next collector refresh changes observed data but must preserve
			// the ownership an operator assigned above.
			if _, err := st.UpsertWorkload(ctx, store.Workload{
				Name: "checkout", Namespace: "prod", Source: "k8s", RequestCPUMilli: 500,
			}); err != nil {
				t.Fatal(err)
			}
			got, err = st.GetWorkload(ctx, wid)
			if err != nil {
				t.Fatal(err)
			}
			if got.TeamID != team.ID || got.TeamName != "Payments" || got.RequestCPUMilli != 500 {
				t.Fatalf("collector refresh lost ownership: %+v", got)
			}

			team.Owner, team.OnCall = "Grace Hopper", "payments@example.com"
			team, err = st.UpdateTeam(ctx, team)
			if err != nil {
				t.Fatal(err)
			}
			if team.Owner != "Grace Hopper" || team.OnCall != "payments@example.com" {
				t.Fatalf("team update did not round-trip: %+v", team)
			}
			got, err = st.GetWorkload(ctx, wid)
			if err != nil {
				t.Fatal(err)
			}
			if got.TeamOnCall != "payments@example.com" {
				t.Fatalf("workload did not expose updated on-call: %+v", got)
			}

			if err := st.SetWorkloadTeam(ctx, wid, nil); err != nil {
				t.Fatal(err)
			}
			got, err = st.GetWorkload(ctx, wid)
			if err != nil {
				t.Fatal(err)
			}
			if got.TeamID != 0 || got.TeamName != "" || got.TeamOnCall != "" {
				t.Fatalf("unassignment did not clear ownership: %+v", got)
			}
		})
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, ok, err := st.GetSetting(ctx, "alert_routing"); err != nil || ok {
				t.Fatalf("empty setting = ok %v err %v, want false nil", ok, err)
			}
			if err := st.PutSetting(ctx, "alert_routing", `{"default_contact_point":"ops"}`); err != nil {
				t.Fatal(err)
			}
			got, ok, err := st.GetSetting(ctx, "alert_routing")
			if err != nil {
				t.Fatal(err)
			}
			if !ok || got != `{"default_contact_point":"ops"}` {
				t.Fatalf("setting = %q ok %v", got, ok)
			}
			if err := st.PutSetting(ctx, "alert_routing", `{"default_contact_point":"platform"}`); err != nil {
				t.Fatal(err)
			}
			got, ok, err = st.GetSetting(ctx, "alert_routing")
			if err != nil {
				t.Fatal(err)
			}
			if !ok || got != `{"default_contact_point":"platform"}` {
				t.Fatalf("updated setting = %q ok %v", got, ok)
			}
		})
	}
}

// TestDBSurfaceRoundTrip exercises the M3 DB surface (ADR-030): DB
// workloads round-trip their class metadata through the upsert conflict
// path, class recommendations round-trip their class pair, and apply
// events carry the class diff through the JSONB column.
func TestDBSurfaceRoundTrip(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			db := store.Workload{
				Name: "payments-prod", Namespace: "db", Kind: "database",
				Source:              "db",
				DBClass:             "db.t3.large",
				DBReplicas:          1,
				DBMaintenanceWindow: "mon:02:00-mon:03:00",
				DBProvider:          "aws",
			}
			id, err := st.UpsertWorkload(ctx, db)
			if err != nil {
				t.Fatal(err)
			}

			// Re-upsert with a changed class must update, not duplicate.
			db.DBClass = "db.t3.xlarge"
			id2, err := st.UpsertWorkload(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if id2 != id {
				t.Fatalf("DB re-upsert changed ID: %d → %d", id, id2)
			}
			got, err := st.GetWorkload(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.DBClass != "db.t3.xlarge" || got.DBReplicas != 1 ||
				got.DBMaintenanceWindow != "mon:02:00-mon:03:00" || got.DBProvider != "aws" {
				t.Fatalf("DB fields did not round-trip: %+v", got)
			}

			// k8s workloads keep the DB fields empty (migration defaults).
			k8sID, err := st.UpsertWorkload(ctx, store.Workload{
				Name: "api", Namespace: "prod", Kind: "deployment", Source: "k8s",
			})
			if err != nil {
				t.Fatal(err)
			}
			kw, err := st.GetWorkload(ctx, k8sID)
			if err != nil {
				t.Fatal(err)
			}
			if kw.DBClass != "" || kw.DBReplicas != 0 || kw.DBMaintenanceWindow != "" || kw.DBProvider != "" {
				t.Fatalf("k8s workload gained DB fields: %+v", kw)
			}

			// Class recommendation round-trip.
			rec := store.Recommendation{
				WorkloadID:     id,
				Resource:       store.ResourceClass,
				ClassCurrent:   "db.t3.xlarge",
				ClassProposed:  "db.t3.medium",
				SavingsMonthly: 50,
				Confidence:     0.9,
				PolicyVersion:  "m3-golden",
			}
			if err := st.CreateRecommendations(ctx, []store.Recommendation{rec}); err != nil {
				t.Fatal(err)
			}
			list, _, err := st.ListRecommendations(ctx, nil, "", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0].ClassCurrent != "db.t3.xlarge" || list[0].ClassProposed != "db.t3.medium" {
				t.Fatalf("class recommendation did not round-trip: %+v", list)
			}
			one, err := st.GetRecommendation(ctx, list[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if one.ClassCurrent != "db.t3.xlarge" || one.ClassProposed != "db.t3.medium" {
				t.Fatalf("GetRecommendation lost class pair: %+v", one)
			}

			// Apply event carries the class diff through the JSONB column.
			evID, err := st.CreateApplyEvent(ctx, store.ApplyEvent{
				RecommendationID: one.ID,
				WorkloadID:       id,
				Actor:            "e2e",
				Mode:             "approved",
				Result:           store.EventApplied,
				Diff: store.Diff{
					Resource:      store.ResourceClass,
					ClassCurrent:  "db.t3.xlarge",
					ClassProposed: "db.t3.medium",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			ev, err := st.GetApplyEvent(ctx, evID)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Diff.Resource != store.ResourceClass ||
				ev.Diff.ClassCurrent != "db.t3.xlarge" || ev.Diff.ClassProposed != "db.t3.medium" {
				t.Fatalf("class diff did not round-trip: %+v", ev.Diff)
			}
		})
	}
}

func TestWorkloadUpsertStableID(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			w := store.Workload{Name: "api", Namespace: "prod", Kind: "deployment", Source: "k8s"}
			id1, err := st.UpsertWorkload(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			w.Labels = map[string]string{"team": "payments"}
			w.RequestCPUMilli = 500
			id2, err := st.UpsertWorkload(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			if id1 != id2 {
				t.Fatalf("re-upsert changed ID: %d → %d", id1, id2)
			}
			got, err := st.GetWorkload(ctx, id1)
			if err != nil {
				t.Fatal(err)
			}
			if got.Labels["team"] != "payments" || got.RequestCPUMilli != 500 {
				t.Fatalf("upsert did not update fields: %+v", got)
			}
			all, err := st.ListWorkloads(ctx)
			if err != nil || len(all) != 1 {
				t.Fatalf("list after upsert: n=%d err=%v", len(all), err)
			}
		})
	}
}

func TestGetWorkloadNotFound(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := st.GetWorkload(ctx, 999); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("want ErrNotFound, got %v", err)
			}
		})
	}
}

func TestBucketUpsertIdempotentAndRange(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wid, err := st.UpsertWorkload(ctx, store.Workload{Name: "w", Namespace: "n", Source: "k8s"})
			if err != nil {
				t.Fatal(err)
			}
			ts := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
			b := store.Bucket{WorkloadID: wid, Metric: store.MetricCPUMilli,
				WindowStart: ts, P95: 100, P99: 110, Max: 120, Samples: 3}
			if err := st.UpsertBucket(ctx, b); err != nil {
				t.Fatal(err)
			}
			// Same window again → update, not duplicate.
			b.P95 = 200
			if err := st.UpsertBucket(ctx, b); err != nil {
				t.Fatal(err)
			}
			got, err := st.ListBuckets(ctx, wid, store.MetricCPUMilli, ts.Add(-time.Hour), ts.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].P95 != 200 {
				t.Fatalf("want 1 updated bucket, got %+v", got)
			}
			// Outside range → empty.
			got, err = st.ListBuckets(ctx, wid, store.MetricCPUMilli, ts.Add(time.Hour), ts.Add(2*time.Hour))
			if err != nil || len(got) != 0 {
				t.Fatalf("range filter: n=%d err=%v", len(got), err)
			}
			// Wrong metric → empty.
			got, err = st.ListBuckets(ctx, wid, store.MetricMemBytes, ts.Add(-time.Hour), ts.Add(time.Hour))
			if err != nil || len(got) != 0 {
				t.Fatalf("metric filter: n=%d err=%v", len(got), err)
			}
		})
	}
}

func TestCreateRecommendationsSupersedesPending(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wid1, _ := st.UpsertWorkload(ctx, store.Workload{Name: "a", Namespace: "n", Source: "k8s"})
			wid2, _ := st.UpsertWorkload(ctx, store.Workload{Name: "b", Namespace: "n", Source: "k8s"})

			rec := func(wid int64, resource string, savings float64) store.Recommendation {
				return store.Recommendation{WorkloadID: wid, Resource: resource,
					CurrentValue: 1000, ProposedValue: 500, SavingsMonthly: savings, Status: store.StatusPending}
			}

			if err := st.CreateRecommendations(ctx, []store.Recommendation{
				rec(wid1, store.ResourceCPU, 10), rec(wid2, store.ResourceMemory, 5)}); err != nil {
				t.Fatal(err)
			}
			// Second run: new recommendation for a1-cpu supersedes the old pending one.
			if err := st.CreateRecommendations(ctx, []store.Recommendation{
				rec(wid1, store.ResourceCPU, 12)}); err != nil {
				t.Fatal(err)
			}

			all, _, err := st.ListRecommendations(ctx, nil, "", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 3 {
				t.Fatalf("want 3 rows (1 superseded + 2 active), got %d", len(all))
			}

			pending, _, err := st.ListRecommendations(ctx, nil, store.StatusPending, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 2 {
				t.Fatalf("want 2 pending, got %d: %+v", len(pending), pending)
			}
			// Pending ordered by savings descending.
			if pending[0].SavingsMonthly != 12 || pending[1].SavingsMonthly != 5 {
				t.Fatalf("savings order wrong: %+v", pending)
			}
			// Workload filter.
			onlyA, _, err := st.ListRecommendations(ctx, &wid1, "", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(onlyA) != 2 || onlyA[0].SavingsMonthly != 12 {
				t.Fatalf("workload filter: %+v", onlyA)
			}

			total, n, err := st.SavingsSummary(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != 2 || total != 17 {
				t.Fatalf("summary: total=%v n=%d", total, n)
			}
		})
	}
}

func TestRecommendationsPagination(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wid1, _ := st.UpsertWorkload(ctx, store.Workload{Name: "a", Namespace: "n", Source: "k8s"})
			wid2, _ := st.UpsertWorkload(ctx, store.Workload{Name: "b", Namespace: "n", Source: "k8s"})

			rec := func(wid int64, resource string, savings float64) store.Recommendation {
				return store.Recommendation{WorkloadID: wid, Resource: resource,
					CurrentValue: 1000, ProposedValue: 500, SavingsMonthly: savings, Status: store.StatusPending}
			}
			// Batch 1 (savings 10-40) then batch 2 (60-90) for the same
			// (workload, resource) pairs: batch 1 rows become superseded.
			if err := st.CreateRecommendations(ctx, []store.Recommendation{
				rec(wid1, store.ResourceCPU, 10), rec(wid1, store.ResourceMemory, 20),
				rec(wid2, store.ResourceCPU, 30), rec(wid2, store.ResourceMemory, 40)}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateRecommendations(ctx, []store.Recommendation{
				rec(wid1, store.ResourceCPU, 60), rec(wid1, store.ResourceMemory, 80),
				rec(wid2, store.ResourceCPU, 70), rec(wid2, store.ResourceMemory, 90)}); err != nil {
				t.Fatal(err)
			}
			// 8 rows total (4 superseded + 4 pending), savings descending:
			// 90 80 70 60 (pending) | 40 30 20 10 (superseded).
			page := func(limit, offset int) ([]store.Recommendation, int) {
				recs, total, err := st.ListRecommendations(ctx, nil, "", limit, offset)
				if err != nil {
					t.Fatal(err)
				}
				return recs, total
			}
			savings := func(recs []store.Recommendation) []float64 {
				out := make([]float64, len(recs))
				for i, r := range recs {
					out[i] = r.SavingsMonthly
				}
				return out
			}
			eq := func(got, want []float64) {
				t.Helper()
				if len(got) != len(want) {
					t.Fatalf("page size: got %v want %v", got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("page: got %v want %v", got, want)
					}
				}
			}

			// Pages of 2 preserve the global savings-descending order.
			p1, total := page(2, 0)
			if total != 8 {
				t.Fatalf("total: %d", total)
			}
			eq(savings(p1), []float64{90, 80})
			p2, total := page(2, 2)
			if total != 8 {
				t.Fatalf("total page 2: %d", total)
			}
			eq(savings(p2), []float64{70, 60})
			p3, total := page(2, 6)
			if total != 8 {
				t.Fatalf("total page 3: %d", total)
			}
			eq(savings(p3), []float64{20, 10})
			// Offset past the end: empty page, total still reported.
			last, total := page(2, 8)
			if len(last) != 0 || total != 8 {
				t.Fatalf("offset past end: len=%d total=%d", len(last), total)
			}
			// Filters compose with pagination: status and workload totals
			// are counted before slicing.
			pending, total, err := st.ListRecommendations(ctx, nil, store.StatusPending, 2, 0)
			if err != nil {
				t.Fatal(err)
			}
			if total != 4 {
				t.Fatalf("pending total: %d", total)
			}
			eq(savings(pending), []float64{90, 80})
			onlyA, total, err := st.ListRecommendations(ctx, &wid1, "", 2, 0)
			if err != nil {
				t.Fatal(err)
			}
			// wid1 rows: 80 60 (pending) + 20 10 (superseded).
			if total != 4 {
				t.Fatalf("wid1 total: %d", total)
			}
			eq(savings(onlyA), []float64{80, 60})
			// limit <= 0 means no limit; offset still applies.
			all, total, err := st.ListRecommendations(ctx, nil, "", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 8 || total != 8 {
				t.Fatalf("no limit: len=%d total=%d", len(all), total)
			}
			rest, total, err := st.ListRecommendations(ctx, nil, "", 0, 2)
			if err != nil {
				t.Fatal(err)
			}
			if total != 8 || len(rest) != 6 {
				t.Fatalf("offset without limit: len=%d total=%d", len(rest), total)
			}
			eq(savings(rest), []float64{70, 60, 40, 30, 20, 10})
		})
	}
}

func TestPruneRecommendations(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wid1, _ := st.UpsertWorkload(ctx, store.Workload{Name: "a", Namespace: "n", Source: "k8s"})
			wid2, _ := st.UpsertWorkload(ctx, store.Workload{Name: "b", Namespace: "n", Source: "k8s"})

			rec := func(wid int64, resource string) store.Recommendation {
				return store.Recommendation{WorkloadID: wid, Resource: resource,
					CurrentValue: 1000, ProposedValue: 500, SavingsMonthly: 10, Status: store.StatusPending}
			}
			// Two runs → two superseded + two pending.
			if err := st.CreateRecommendations(ctx, []store.Recommendation{rec(wid1, store.ResourceCPU), rec(wid2, store.ResourceMemory)}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateRecommendations(ctx, []store.Recommendation{rec(wid1, store.ResourceCPU), rec(wid2, store.ResourceMemory)}); err != nil {
				t.Fatal(err)
			}
			// Audit rows that must never be pruned.
			appliedID, err := st.CreateFollowUpRecommendation(ctx, store.Recommendation{
				WorkloadID: wid1, Resource: store.ResourceMemory,
				CurrentValue: 1000, ProposedValue: 500, SavingsMonthly: 10,
				Confidence: 0.9, PolicyVersion: "v1", Status: store.StatusApplied})
			if err != nil {
				t.Fatal(err)
			}
			rolledID, err := st.CreateFollowUpRecommendation(ctx, store.Recommendation{
				WorkloadID: wid2, Resource: store.ResourceCPU,
				CurrentValue: 1000, ProposedValue: 500, SavingsMonthly: 10,
				Confidence: 0.9, PolicyVersion: "v1", Status: store.StatusRolled})
			if err != nil {
				t.Fatal(err)
			}

			// Cutoff in the future: every superseded row is older than it,
			// but non-superseded statuses survive regardless of age.
			n, err := st.PruneRecommendations(ctx, store.StatusSuperseded, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Fatalf("pruned: %d, want 2", n)
			}
			all, total, err := st.ListRecommendations(ctx, nil, "", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if total != 4 {
				t.Fatalf("after prune: total=%d", total)
			}
			for _, r := range all {
				if r.Status == store.StatusSuperseded {
					t.Fatalf("superseded row survived: %+v", r)
				}
			}
			if r, err := st.GetRecommendation(ctx, appliedID); err != nil || r.Status != store.StatusApplied {
				t.Fatalf("applied row damaged: %+v, %v", r, err)
			}
			if r, err := st.GetRecommendation(ctx, rolledID); err != nil || r.Status != store.StatusRolled {
				t.Fatalf("rolled_back row damaged: %+v, %v", r, err)
			}

			// Cutoff in the past: nothing was created that long ago.
			n, err = st.PruneRecommendations(ctx, store.StatusSuperseded, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("past cutoff pruned %d rows", n)
			}
			// The status parameter is a filter, not a constant: pending rows
			// older than the cutoff are pruned too.
			n, err = st.PruneRecommendations(ctx, store.StatusPending, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Fatalf("pending prune: %d, want 2", n)
			}
			all, total, _ = st.ListRecommendations(ctx, nil, "", 0, 0)
			if total != 2 {
				t.Fatalf("final total: %d", total)
			}
			for _, r := range all {
				if r.Status == store.StatusApplied || r.Status == store.StatusRolled {
					continue
				}
				t.Fatalf("unexpected survivor: %+v", r)
			}
		})
	}
}

func TestPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wid, err := st.UpsertWorkload(ctx, store.Workload{
				Name: "api", Namespace: "prod", Source: "k8s",
				RequestCPUMilli: 1000, RequestMemBytes: 1 << 30,
			})
			if err != nil {
				t.Fatal(err)
			}
			base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < 96; i++ {
				if err := st.UpsertBucket(ctx, store.Bucket{
					WorkloadID: wid, Metric: store.MetricCPUMilli,
					WindowStart: base.Add(time.Duration(i) * 15 * time.Minute),
					P95:         300, P99: 320, Max: 400, Samples: 2,
				}); err != nil {
					t.Fatal(err)
				}
			}
			// Buckets ordered by window_start.
			got, err := st.ListBuckets(ctx, wid, store.MetricCPUMilli, time.Time{}, time.Now().Add(365*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 96 || !got[0].WindowStart.Before(got[95].WindowStart) {
				t.Fatalf("bucket order: %+v", got)
			}
		})
	}
}
