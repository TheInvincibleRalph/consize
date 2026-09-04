// Command injector is the interactive demo companion for Consize.
//
// It watches GET /api/v1/applies on the Consize API. When it sees a
// new apply event in "applied" status, it waits CONSIZE_DEMO_INJECT_DELAY
// (default 15s) then signals the prometheus-stub to inject OOMKill
// failures for the workload's namespace. The real Consize verifier then
// detects the breach on its next tick and issues an automatic rollback.
// After the verifier records a verdict, the injector clears the failure
// mode so the stub returns clean data again.
//
// This keeps the verifier's code path completely unchanged — it queries
// Prometheus exactly as in production.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

type applyEvent struct {
	ID         int64  `json:"ID"`
	WorkloadID int64  `json:"WorkloadID"`
	Result     string `json:"Result"` // planned | applied | reverted
	Actor      string `json:"Actor"`
	CreatedAt  string `json:"CreatedAt"`
}

type appliesResponse struct {
	Applies []applyEvent `json:"applies"`
}

type workload struct {
	ID        int64  `json:"ID"`
	Namespace string `json:"Namespace"`
}

type workloadsResponse struct {
	Workloads []workload `json:"workloads"`
}

func main() {
	apiBase := getenv("CONSIZE_API_URL", "http://api:8080")
	stubBase := getenv("CONSIZE_STUB_URL", "http://prometheus-stub:9090")
	pollInterval := parseDuration("CONSIZE_DEMO_POLL_INTERVAL", 5*time.Second)
	injectDelay := parseDuration("CONSIZE_DEMO_INJECT_DELAY", 15*time.Second)

	log.Printf("injector started — api=%s stub=%s poll=%s injectDelay=%s",
		apiBase, stubBase, pollInterval, injectDelay)

	// Track which apply IDs we have already handled so we don't double-inject.
	handled := map[int64]bool{}

	for {
		time.Sleep(pollInterval)

		events, err := listApplies(apiBase)
		if err != nil {
			log.Printf("poll error: %v", err)
			continue
		}

		for _, ev := range events {
			if handled[ev.ID] {
				continue
			}
			if ev.Result != "applied" {
				continue
			}
			handled[ev.ID] = true
			
			// Lookup namespace
			ns := getNamespace(apiBase, ev.WorkloadID)
			if ns == "" {
				log.Printf("injector: unknown namespace for workload %d, skipping", ev.WorkloadID)
				continue
			}

			go func(ev applyEvent, namespace string) {
				log.Printf("injector: detected apply %d for namespace %q — injecting failure in %s",
					ev.ID, namespace, injectDelay)
				time.Sleep(injectDelay)

				// Signal the stub to start returning OOMKill events.
				if err := stubPost(stubBase+"/inject/"+namespace); err != nil {
					log.Printf("inject signal failed: %v", err)
					return
				}
				log.Printf("injector: OOMKill failure active for namespace %q", namespace)

				// Poll until the verifier issues a verdict, then clear.
				for range 60 {
					time.Sleep(pollInterval)
					verdict, err := getVerdict(apiBase, ev.ID)
					if err != nil {
						log.Printf("verdict poll error: %v", err)
						continue
					}
					if verdict == "failed" || verdict == "passed" || verdict == "inconclusive" {
						log.Printf("injector: apply %d verdict=%s — clearing failure for %q",
							ev.ID, verdict, namespace)
						_ = stubPost(stubBase + "/clear/" + namespace)
						return
					}
				}
				// Safety cleanup: clear after 5 minutes regardless.
				log.Printf("injector: timeout — clearing failure for %q", namespace)
				_ = stubPost(stubBase + "/clear/" + namespace)
			}(ev, ns)
		}
	}
}

func listApplies(apiBase string) ([]applyEvent, error) {
	resp, err := http.Get(apiBase + "/api/v1/applies")
	if err != nil {
		return nil, fmt.Errorf("GET applies: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		// Demo mode runs with auth disabled. If auth is somehow enabled,
		// log it clearly rather than silently ignoring events.
		return nil, fmt.Errorf("applies endpoint returned 401 — is CONSIZE_AUTH_REQUIRED=false set?")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("applies: unexpected status %d", resp.StatusCode)
	}
	var body appliesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode applies: %w", err)
	}
	return body.Applies, nil
}

func getNamespace(apiBase string, workloadID int64) string {
	resp, err := http.Get(apiBase + "/api/v1/workloads")
	if err != nil {
		log.Printf("GET workloads error: %v", err)
		return ""
	}
	defer resp.Body.Close()
	var body workloadsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("decode workloads error: %v", err)
		return ""
	}
	for _, w := range body.Workloads {
		if w.ID == workloadID {
			return w.Namespace
		}
	}
	return ""
}

// getVerdict fetches the verification runs for a specific apply event
// and returns the verdict string if one exists.
func getVerdict(apiBase string, applyID int64) (string, error) {
	resp, err := http.Get(apiBase + "/api/v1/verification-runs")
	if err != nil {
		return "", fmt.Errorf("GET verification-runs: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Runs []struct {
			ApplyEventID int64  `json:"ApplyEventID"`
			Verdict      string `json:"Verdict"`
		} `json:"verification_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode verification-runs: %w", err)
	}
	for _, r := range body.Runs {
		if r.ApplyEventID == applyID && r.Verdict != "" {
			return r.Verdict, nil
		}
	}
	return "", nil
}

func stubPost(url string) error {
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stub POST %s: status %d", url, resp.StatusCode)
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		// Accept seconds as a plain integer for simplicity in docker-compose.
		if s, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Duration(s) * time.Second
		}
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

