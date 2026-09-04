package costscan

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"consize/internal/store"
)

func TestGCPSourceScansDetachedDisksAndStoppedInstances(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/compute/v1/projects/acme/aggregated/disks", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"items": {
				"zones/us-central1-a": {
					"disks": [
						{
							"id": "123",
							"name": "old-cache",
							"selfLink": "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a/disks/old-cache",
							"zone": "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a",
							"type": "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a/diskTypes/pd-balanced",
							"status": "READY",
							"sizeGb": "100",
							"users": [],
							"creationTimestamp": "2026-08-01T00:00:00.000-00:00"
						},
						{
							"id": "456",
							"name": "attached",
							"status": "READY",
							"sizeGb": "200",
							"users": ["vm-1"]
						}
					]
				}
			}
		}`))
	})
	mux.HandleFunc("/compute/v1/projects/acme/aggregated/instances", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"items": {
				"zones/us-central1-a": {
					"instances": [
						{
							"id": "789",
							"name": "qa-runner",
							"selfLink": "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a/instances/qa-runner",
							"zone": "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a",
							"status": "TERMINATED",
							"creationTimestamp": "2026-08-02T00:00:00.000-00:00",
							"disks": [
								{"type": "PERSISTENT", "boot": true, "diskSizeGb": "50"}
							]
						},
						{
							"id": "790",
							"name": "running",
							"status": "RUNNING",
							"disks": [
								{"type": "PERSISTENT", "diskSizeGb": "50"}
							]
						}
					]
				}
			}
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := &GCPSource{
		project:   "acme",
		base:      srv.URL,
		client:    srv.Client(),
		tokenFunc: func(context.Context) (string, error) { return "test-token", nil },
		now:       func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) },
	}
	opps, err := src.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(opps) != 2 {
		t.Fatalf("want 2 opportunities, got %d: %+v", len(opps), opps)
	}
	if opps[0].ResourceType != TypeUnattachedVolume || opps[0].Name != "old-cache" || opps[0].MonthlyCost != 10 {
		t.Fatalf("bad disk opportunity: %+v", opps[0])
	}
	if opps[1].ResourceType != TypeStoppedInstance || opps[1].Name != "qa-runner" || opps[1].MonthlyCost != 5 {
		t.Fatalf("bad stopped instance opportunity: %+v", opps[1])
	}
	if !strings.HasPrefix(opps[0].TerraformAddr, "google_compute_disk.") ||
		!strings.HasPrefix(opps[1].TerraformAddr, "google_compute_instance.") {
		t.Fatalf("bad terraform addrs: %q %q", opps[0].TerraformAddr, opps[1].TerraformAddr)
	}
}

func TestGCPSourceDirectApplyDeletesOnlyDetachedDisk(t *testing.T) {
	var sawDelete bool
	mux := http.NewServeMux()
	mux.HandleFunc("/compute/v1/projects/acme/zones/us-central1-a/disks/old-cache", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth header = %q", got)
		}
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"name": "old-cache",
				"selfLink": "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a/disks/old-cache",
				"zone": "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a",
				"status": "READY",
				"users": []
			}`))
		case http.MethodDelete:
			sawDelete = true
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"PENDING"}`))
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := &GCPSource{
		project:   "acme",
		base:      srv.URL,
		client:    srv.Client(),
		tokenFunc: func(context.Context) (string, error) { return "test-token", nil },
	}
	opp := store.CostOpportunity{
		ID:           7,
		Provider:     "gcp",
		ResourceType: TypeUnattachedVolume,
		ResourceID:   "https://compute.googleapis.com/compute/v1/projects/acme/zones/us-central1-a/disks/old-cache",
		Name:         "old-cache",
		Action:       "delete_disk",
	}
	plan, err := src.ApplyDirect(context.Background(), opp, "dry_run")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied || sawDelete {
		t.Fatalf("dry run must not delete: %+v sawDelete=%v", plan, sawDelete)
	}
	res, err := src.ApplyDirect(context.Background(), opp, "approved")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || !sawDelete {
		t.Fatalf("approved cleanup did not delete: %+v sawDelete=%v", res, sawDelete)
	}
}
