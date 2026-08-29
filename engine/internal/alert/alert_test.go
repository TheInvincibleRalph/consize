package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type captureTransport struct {
	mu       sync.Mutex
	requests map[string]int
	bodies   []map[string]any
}

func newCaptureTransport() *captureTransport {
	return &captureTransport{requests: map[string]int{}}
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.requests[req.URL.String()]++
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader([]byte("ok"))),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func TestLegacySlackWebhookBecomesDefaultContactPoint(t *testing.T) {
	rt := newCaptureTransport()
	t.Setenv(envSlackWebhook, "http://slack.test/default")
	n := New()
	n.client = &http.Client{Transport: rt}

	n.Notify(context.Background(), "plain compatibility alert")

	if len(rt.bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(rt.bodies))
	}
	got := rt.bodies[0]
	if got["text"] == "" {
		t.Fatalf("payload missing fallback text: %#v", got)
	}
	if blocks, ok := got["blocks"].([]any); !ok || len(blocks) == 0 {
		t.Fatalf("payload missing slack blocks: %#v", got)
	}
}

func TestNotificationPolicyRoutesByLabels(t *testing.T) {
	rt := newCaptureTransport()
	cfg := `{
		"default_contact_point": "fallback",
		"contact_points": [
			{"name":"critical","integrations":[{"type":"slack","webhook_url":"http://slack.test/critical","mention":"<!subteam^S123>"}]},
			{"name":"fallback","integrations":[{"type":"slack","webhook_url":"http://slack.test/fallback"}]}
		],
		"notification_policies": [
			{"name":"critical-verification","match":{"severity":"critical","alertname":"ConsizeVerificationFailed"},"contact_point":"critical"}
		]
	}`
	t.Setenv(envRoutingConfig, cfg)
	n := New()
	n.client = &http.Client{Transport: rt}

	n.NotifyEvent(context.Background(), Event{
		Title:   "verification failed",
		Summary: "boutique/frontend failed",
		Labels:  map[string]string{"severity": "critical", "alertname": "ConsizeVerificationFailed"},
	})
	n.NotifyEvent(context.Background(), Event{
		Title:   "warning",
		Summary: "collector stale",
		Labels:  map[string]string{"severity": "warning", "alertname": "ConsizeCollectorStale"},
	})

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.requests["http://slack.test/critical"] != 1 {
		t.Fatalf("critical hits = %d, want 1", rt.requests["http://slack.test/critical"])
	}
	if rt.requests["http://slack.test/fallback"] != 1 {
		t.Fatalf("fallback hits = %d, want 1", rt.requests["http://slack.test/fallback"])
	}
}

func TestNotificationPolicyContinueRoutesToMultipleContactPoints(t *testing.T) {
	rt := newCaptureTransport()
	cfg := `{
		"contact_points": [
			{"name":"a","integrations":[{"type":"slack","webhook_url":"http://slack.test/a"}]},
			{"name":"b","integrations":[{"type":"slack","webhook_url":"http://slack.test/b"}]}
		],
		"notification_policies": [
			{"name":"first","match":{"severity":"critical"},"contact_point":"a","continue":true},
			{"name":"second","match":{"surface":"db"},"contact_point":"b"}
		]
	}`
	t.Setenv(envRoutingConfig, cfg)
	n := New()
	n.client = &http.Client{Transport: rt}

	n.NotifyEvent(context.Background(), Event{
		Title:   "db rollback failed",
		Summary: "rollback failed",
		Labels:  map[string]string{"severity": "critical", "surface": "db"},
	})

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.bodies) != 2 {
		t.Fatalf("hits = %d, want 2", len(rt.bodies))
	}
}

func TestSlackPayloadIncludesMentionAndContext(t *testing.T) {
	payload := slackPayload(Event{
		Title:    "Consize verification failed",
		Summary:  "boutique/frontend failed",
		DedupKey: "consize:boutique/frontend:42",
		Labels: map[string]string{
			"namespace": "boutique",
			"workload":  "frontend",
			"severity":  "critical",
		},
		Annotations: map[string]string{
			"dashboard_url": "http://localhost:3000/workloads/7",
			"rollback":      "automatic rollback started",
		},
	}, ContactPoint{Name: "ops-slack"}, Integration{Mention: "<!subteam^S123>", Channel: "#platform-oncall"})

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"subteam^S123", "boutique", "frontend", "automatic rollback started", "ops-slack", "#platform-oncall"} {
		if !strings.Contains(body, want) {
			t.Fatalf("payload missing %q: %s", want, body)
		}
	}
}
