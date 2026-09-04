// Command prometheus-stub is a minimal Prometheus HTTP API stub for the
// Consize interactive demo sandbox.
//
// It serves /api/v1/query_range and returns clean SLI data normally.
// The injector signals it via POST /inject/{namespace} to start
// returning OOMKill events for that namespace, triggering a verification
// FAIL and automatic rollback. POST /clear/{namespace} restores clean data.
//
// This keeps the real verifier binary completely unchanged — it queries
// the stub exactly as it would a real Prometheus instance.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	mu           sync.RWMutex
	failingNS    = map[string]bool{} // namespaces currently in failure mode
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query_range", handleQueryRange)
	mux.HandleFunc("/inject/", handleInject)
	mux.HandleFunc("/clear/", handleClear)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := ":9090"
	log.Printf("prometheus-stub listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// handleInject marks a namespace as failing — the stub will return
// elevated OOMKill counter values for all subsequent queries.
func handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ns := strings.TrimPrefix(r.URL.Path, "/inject/")
	if ns == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	mu.Lock()
	failingNS[ns] = true
	mu.Unlock()
	log.Printf("[stub] injecting OOMKill failure for namespace %q", ns)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","namespace":%q}`, ns)
}

// handleClear restores clean data for a namespace.
func handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ns := strings.TrimPrefix(r.URL.Path, "/clear/")
	mu.Lock()
	delete(failingNS, ns)
	mu.Unlock()
	log.Printf("[stub] cleared failure mode for namespace %q", ns)
	w.WriteHeader(http.StatusOK)
}

// handleQueryRange implements /api/v1/query_range.
// It returns:
//   - Clean data (throttling ~0, OOM=0, restarts=0) by default.
//   - Elevated OOMKill events when the query's namespace is in failingNS.
func handleQueryRange(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
	end, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
	step, _ := strconv.ParseInt(r.URL.Query().Get("step"), 10, 64)

	if step <= 0 {
		step = 60
	}
	if end <= start {
		end = start + step
	}

	// Determine if a failing namespace is referenced in this query.
	mu.RLock()
	failing := false
	failingNamespace := ""
	for ns := range failingNS {
		if strings.Contains(query, ns) || strings.Contains(query, `namespace="`+ns+`"`) {
			failing = true
			failingNamespace = ns
			break
		}
	}
	mu.RUnlock()

	// Build time series values.
	var points [][2]any
	for t := start; t <= end; t += step {
		var v float64
		if failing && isOOMQuery(query) {
			// 3 OOMKill events in 5-minute windows — well above the 0-threshold.
			v = 3.0
		} else if failing && isThrottleQuery(query) {
			// Elevated throttling: ~45% — above the baseline but not the
			// primary trigger (OOM is more dramatic for the demo).
			v = 0.45
		} else {
			// Clean baseline: tiny non-zero throttling is realistic.
			v = 0.001
		}
		points = append(points, [2]any{float64(t), fmt.Sprintf("%.6f", v)})
	}

	labelKey := "namespace"
	labelVal := failingNamespace
	if labelVal == "" {
		// Extract namespace from query for the label — best-effort.
		if idx := strings.Index(query, `namespace="`); idx >= 0 {
			rest := query[idx+len(`namespace="`):]
			if end2 := strings.Index(rest, `"`); end2 >= 0 {
				labelVal = rest[:end2]
			}
		}
	}
	if labelVal == "" {
		labelVal = "demo"
	}

	resp := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result": []map[string]any{
				{
					"metric": map[string]string{labelKey: labelVal},
					"values": points,
				},
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func isOOMQuery(q string) bool {
	return strings.Contains(q, "oom") || strings.Contains(q, "OOM")
}

func isThrottleQuery(q string) bool {
	return strings.Contains(q, "throttl")
}

// ensure time import is used (used for future extension).
var _ = time.Second

