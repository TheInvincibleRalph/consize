// Package alert is Consize's outbound notification router.
//
// It intentionally mirrors the Grafana Alerting model: alert events carry
// labels, notification policies match those labels, and matched policies send
// to named contact points. The first concrete integration is Slack webhook
// delivery; provider-backed incident creation (PagerDuty/JSM) uses the same
// event seam later.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	envRoutingConfig = "CONSIZE_ALERT_ROUTING"
	envSlackWebhook  = "CONSIZE_SLACK_WEBHOOK"
	envInstallID     = "CONSIZE_INSTALLATION_ID"
	StoreRoutingKey  = "alert_routing"
)

// Event is the provider-neutral alert payload emitted by Consize safety
// workflows. Labels are for routing; annotations are for human context.
type Event struct {
	Title       string            `json:"title"`
	Summary     string            `json:"summary"`
	Status      string            `json:"status"` // firing | resolved
	DedupKey    string            `json:"dedup_key"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// RoutingConfig is loaded from CONSIZE_ALERT_ROUTING. Example:
//
//	{
//	  "default_contact_point": "ops-slack",
//	  "contact_points": [
//	    {"name":"ops-slack","integrations":[
//	      {"type":"slack","webhook_env":"CONSIZE_SLACK_WEBHOOK","mention":"<!subteam^S123>"}
//	    ]}
//	  ],
//	  "notification_policies": [
//	    {"name":"critical","match":{"severity":"critical"},"contact_point":"ops-slack"}
//	  ]
//	}
//
// Secrets stay out of the JSON by using webhook_env, which points at a
// Kubernetes Secret-backed environment variable.
type RoutingConfig struct {
	DefaultContactPoint string               `json:"default_contact_point"`
	ContactPoints       []ContactPoint       `json:"contact_points"`
	Policies            []NotificationPolicy `json:"notification_policies"`
}

type ContactPoint struct {
	Name         string        `json:"name"`
	Integrations []Integration `json:"integrations"`
}

type Integration struct {
	Type       string `json:"type"`        // slack
	WebhookURL string `json:"webhook_url"` // tests/dev only; prefer WebhookEnv
	WebhookEnv string `json:"webhook_env"`
	Mention    string `json:"mention"` // stable Slack user/group mention, e.g. <@U...> or <!subteam^S...>
	Channel    string `json:"channel"` // human label; incoming webhooks are already bound to a channel
}

type NotificationPolicy struct {
	Name         string            `json:"name"`
	Match        map[string]string `json:"match"`
	ContactPoint string            `json:"contact_point"`
	Continue     bool              `json:"continue"`
}

type ConfigProvider interface {
	GetSetting(ctx context.Context, key string) (value string, ok bool, err error)
}

type DeliveryResult struct {
	ContactPoint string `json:"contact_point"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

// Notifier sends operational alerts (verdicts, rollbacks, failures).
type Notifier struct {
	cfg      RoutingConfig
	provider ConfigProvider
	client   *http.Client
	log      *slog.Logger
}

// New builds a notifier from the environment.
func New() *Notifier {
	cfg, err := ConfigFromEnv()
	n := &Notifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
		log:    slog.Default(),
	}
	if err != nil {
		n.log.Error("alert routing config invalid; notifications will log only", "err", err)
		n.cfg = RoutingConfig{}
	}
	return n
}

// NewWithConfigProvider builds a notifier that reads durable routing config
// before falling back to environment provisioning.
func NewWithConfigProvider(provider ConfigProvider) *Notifier {
	n := New()
	n.provider = provider
	return n
}

func NewWithConfig(cfg RoutingConfig) *Notifier {
	return &Notifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
		log:    slog.Default(),
	}
}

// ConfigFromEnv loads Grafana-style routing config, falling back to the legacy
// single Slack webhook as the default contact point.
func ConfigFromEnv() (RoutingConfig, error) {
	raw := strings.TrimSpace(os.Getenv(envRoutingConfig))
	if raw != "" {
		var cfg RoutingConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return RoutingConfig{}, fmt.Errorf("%s: %w", envRoutingConfig, err)
		}
		if err := cfg.Validate(); err != nil {
			return RoutingConfig{}, err
		}
		return cfg, nil
	}
	if os.Getenv(envSlackWebhook) == "" {
		return RoutingConfig{}, nil
	}
	return RoutingConfig{
		DefaultContactPoint: "default-slack",
		ContactPoints: []ContactPoint{{
			Name: "default-slack",
			Integrations: []Integration{{
				Type:       "slack",
				WebhookEnv: envSlackWebhook,
			}},
		}},
		Policies: []NotificationPolicy{{
			Name:         "default",
			ContactPoint: "default-slack",
		}},
	}, nil
}

func ParseRoutingConfig(raw string) (RoutingConfig, error) {
	var cfg RoutingConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return RoutingConfig{}, err
	}
	if err := cfg.Validate(); err != nil {
		return RoutingConfig{}, err
	}
	return cfg, nil
}

func (c RoutingConfig) Validate() error {
	names := map[string]bool{}
	for _, cp := range c.ContactPoints {
		if cp.Name == "" {
			return fmt.Errorf("contact point name is required")
		}
		if names[cp.Name] {
			return fmt.Errorf("duplicate contact point %q", cp.Name)
		}
		names[cp.Name] = true
		if len(cp.Integrations) == 0 {
			return fmt.Errorf("contact point %q has no integrations", cp.Name)
		}
		for _, in := range cp.Integrations {
			if strings.ToLower(in.Type) != "slack" {
				return fmt.Errorf("contact point %q has unsupported integration type %q", cp.Name, in.Type)
			}
			if in.WebhookURL == "" && in.WebhookEnv == "" {
				return fmt.Errorf("slack integration in contact point %q needs webhook_env", cp.Name)
			}
		}
	}
	if c.DefaultContactPoint != "" && !names[c.DefaultContactPoint] {
		return fmt.Errorf("default contact point %q not found", c.DefaultContactPoint)
	}
	for _, p := range c.Policies {
		if p.ContactPoint == "" {
			return fmt.Errorf("policy %q needs contact_point", p.Name)
		}
		if !names[p.ContactPoint] {
			return fmt.Errorf("policy %q references unknown contact point %q", p.Name, p.ContactPoint)
		}
	}
	return nil
}

// Notify logs the message and routes it through the default notification path.
// This preserves the old alert API while newer safety code emits structured
// events with labels and annotations.
func (n *Notifier) Notify(ctx context.Context, msg string) {
	n.NotifyEvent(ctx, Event{
		Title:    msg,
		Summary:  msg,
		Status:   "firing",
		DedupKey: dedupFrom(msg),
		Labels: map[string]string{
			"alertname": "ConsizeAlert",
			"severity":  "warning",
		},
	})
}

// NotifyEvent logs and delivers an alert event. Delivery failures are logged,
// never fatal — the audit trail remains the source of truth.
func (n *Notifier) NotifyEvent(ctx context.Context, ev Event) {
	_ = n.DeliverEvent(ctx, ev)
}

// DeliverEvent logs and delivers an alert event, returning per-integration
// delivery status for API "test contact point" flows.
func (n *Notifier) DeliverEvent(ctx context.Context, ev Event) []DeliveryResult {
	ev = normalizeEvent(ev)
	n.log.Warn("alert: "+ev.Title, "dedup_key", ev.DedupKey, "labels", ev.Labels)

	cfg := n.routingConfig(ctx)
	contactPoints := route(cfg, ev)
	results := []DeliveryResult{}
	for _, cp := range contactPoints {
		for _, in := range cp.Integrations {
			if strings.ToLower(in.Type) != "slack" {
				continue
			}
			result := DeliveryResult{ContactPoint: cp.Name, Type: in.Type, Status: "sent"}
			if err := n.deliverSlack(ctx, ev, cp, in); err != nil {
				result.Status = "failed"
				result.Error = err.Error()
				n.log.Error("alert delivery failed", "contact_point", cp.Name, "type", in.Type, "err", err)
			}
			results = append(results, result)
		}
	}
	return results
}

func normalizeEvent(ev Event) Event {
	if ev.Title == "" {
		ev.Title = "Consize alert"
	}
	if ev.Summary == "" {
		ev.Summary = ev.Title
	}
	if ev.Status == "" {
		ev.Status = "firing"
	}
	if ev.DedupKey == "" {
		ev.DedupKey = dedupFrom(ev.Title + "|" + ev.Summary)
	}
	if ev.Labels == nil {
		ev.Labels = map[string]string{}
	}
	if ev.Annotations == nil {
		ev.Annotations = map[string]string{}
	}
	if ev.Labels["installation"] == "" {
		if id := os.Getenv(envInstallID); id != "" {
			ev.Labels["installation"] = id
		}
	}
	return ev
}

func (n *Notifier) routingConfig(ctx context.Context) RoutingConfig {
	if n.provider == nil {
		return n.cfg
	}
	raw, ok, err := n.provider.GetSetting(ctx, StoreRoutingKey)
	if err != nil {
		n.log.Error("alert routing config unavailable; falling back to env", "err", err)
		return n.cfg
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return n.cfg
	}
	cfg, err := ParseRoutingConfig(raw)
	if err != nil {
		n.log.Error("stored alert routing config invalid; falling back to env", "err", err)
		return n.cfg
	}
	return cfg
}

func route(cfg RoutingConfig, ev Event) []ContactPoint {
	if len(cfg.ContactPoints) == 0 {
		return nil
	}
	byName := map[string]ContactPoint{}
	for _, cp := range cfg.ContactPoints {
		byName[cp.Name] = cp
	}

	var out []ContactPoint
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if cp, ok := byName[name]; ok {
			out = append(out, cp)
			seen[name] = true
		}
	}

	matched := false
	for _, p := range cfg.Policies {
		if !matches(ev.Labels, p.Match) {
			continue
		}
		matched = true
		add(p.ContactPoint)
		if !p.Continue {
			break
		}
	}
	if !matched {
		add(cfg.DefaultContactPoint)
	}
	return out
}

func matches(labels, match map[string]string) bool {
	for k, want := range match {
		if labels[k] != want {
			return false
		}
	}
	return true
}

func (n *Notifier) deliverSlack(ctx context.Context, ev Event, cp ContactPoint, in Integration) error {
	webhook := in.WebhookURL
	if webhook == "" && in.WebhookEnv != "" {
		webhook = os.Getenv(in.WebhookEnv)
	}
	if webhook == "" {
		return fmt.Errorf("slack webhook is empty")
	}
	body, err := json.Marshal(slackPayload(ev, cp, in))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("slack status %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

type slackText struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

type slackBlock struct {
	Type     string      `json:"type"`
	Text     *slackText  `json:"text,omitempty"`
	Fields   []slackText `json:"fields,omitempty"`
	Elements []slackText `json:"elements,omitempty"`
}

func slackPayload(ev Event, cp ContactPoint, in Integration) map[string]any {
	title := truncate(ev.Title, 150)
	summary := ev.Summary
	if in.Mention != "" && !strings.Contains(summary, in.Mention) {
		summary = in.Mention + " " + summary
	}
	fields := []slackText{}
	for _, k := range []string{"installation", "namespace", "workload", "resource", "severity", "status"} {
		if v := ev.Labels[k]; v != "" {
			fields = append(fields, slackText{Type: "mrkdwn", Text: fmt.Sprintf("*%s*\n`%s`", titleCase(k), v)})
		}
	}
	for _, k := range []string{"failed_signal", "change", "rollback", "dashboard_url", "provider_incident_url"} {
		if v := ev.Annotations[k]; v != "" {
			fields = append(fields, slackText{Type: "mrkdwn", Text: fmt.Sprintf("*%s*\n%s", titleCase(k), v)})
		}
	}
	if len(fields) > 10 {
		fields = fields[:10]
	}

	context := []string{"dedup: `" + ev.DedupKey + "`", "contact point: `" + cp.Name + "`"}
	if in.Channel != "" {
		context = append(context, "channel: `"+in.Channel+"`")
	}

	return map[string]any{
		"text": title + " — " + ev.Summary,
		"blocks": []slackBlock{
			{Type: "header", Text: &slackText{Type: "plain_text", Text: title, Emoji: true}},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: summary}},
			{Type: "section", Fields: fields},
			{Type: "context", Elements: []slackText{{Type: "mrkdwn", Text: strings.Join(context, " · ")}}},
		},
	}
}

func titleCase(s string) string {
	parts := strings.Split(strings.ReplaceAll(s, "_", " "), " ")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func dedupFrom(s string) string {
	parts := strings.Fields(strings.ToLower(s))
	if len(parts) == 0 {
		return "consize-alert"
	}
	sort.Strings(parts)
	key := strings.Join(parts, "-")
	key = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == ':' || r == '/' {
			return r
		}
		return '-'
	}, key)
	return truncate(key, 160)
}
