package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"consize/internal/alert"
)

type alertingConfigResponse struct {
	Config alert.RoutingConfig `json:"config"`
	Source string              `json:"source"`
}

func (s *Server) getAlertingConfig(w http.ResponseWriter, r *http.Request) {
	cfg, source, err := s.alertingConfig(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, alertingConfigResponse{Config: sanitizeAlertConfig(cfg), Source: source})
}

func (s *Server) putAlertingConfig(w http.ResponseWriter, r *http.Request) {
	var cfg alert.RoutingConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be an alert routing config"})
		return
	}
	cfg = sanitizeAlertConfig(cfg)
	if err := rejectStoredSecrets(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := cfg.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.store.PutSetting(r.Context(), alert.StoreRoutingKey, string(raw)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, alertingConfigResponse{Config: cfg, Source: "store"})
}

func (s *Server) testAlertingConfig(w http.ResponseWriter, r *http.Request) {
	cfg, badRequest, err := s.alertingTestConfig(r)
	if err != nil {
		if badRequest {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeErr(w, err)
		return
	}
	n := alert.NewWithConfig(cfg)
	results := n.DeliverEvent(r.Context(), alert.Event{
		Title:    "Consize test notification",
		Summary:  "This is a test notification from Consize alerting configuration.",
		Status:   "firing",
		DedupKey: "consize:test-notification",
		Labels: map[string]string{
			"alertname": "ConsizeVerificationFailed",
			"severity":  "critical",
			"namespace": "demo",
			"workload":  "routing-check",
			"resource":  "memory",
			"surface":   "k8s",
			"status":    "firing",
		},
		Annotations: map[string]string{
			"change":        "test only — no workload changes",
			"rollback":      "not applicable",
			"dashboard_url": "open Consize dashboard",
		},
	})
	code := http.StatusOK
	if len(results) == 0 {
		code = http.StatusBadRequest
	}
	for _, result := range results {
		if result.Status == "failed" {
			code = http.StatusBadGateway
			break
		}
	}
	writeJSON(w, code, map[string]any{"results": results})
}

func (s *Server) alertingTestConfig(r *http.Request) (alert.RoutingConfig, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return alert.RoutingConfig{}, false, err
	}
	if strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "{}" {
		cfg, _, err := s.alertingConfig(r.Context())
		return cfg, false, err
	}
	var cfg alert.RoutingConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return alert.RoutingConfig{}, true, fmt.Errorf("body must be an alert routing config")
	}
	cfg = sanitizeAlertConfig(cfg)
	if err := rejectStoredSecrets(cfg); err != nil {
		return alert.RoutingConfig{}, true, err
	}
	if err := cfg.Validate(); err != nil {
		return alert.RoutingConfig{}, true, err
	}
	return cfg, false, nil
}

func (s *Server) alertingConfig(ctx context.Context) (alert.RoutingConfig, string, error) {
	if raw, ok, err := s.store.GetSetting(ctx, alert.StoreRoutingKey); err != nil {
		return alert.RoutingConfig{}, "", err
	} else if ok && strings.TrimSpace(raw) != "" {
		cfg, err := alert.ParseRoutingConfig(raw)
		if err != nil {
			return alert.RoutingConfig{}, "", fmt.Errorf("stored alert routing config: %w", err)
		}
		return cfg, "store", nil
	}
	cfg, err := alert.ConfigFromEnv()
	if err != nil {
		return alert.RoutingConfig{}, "", err
	}
	return cfg, "env", nil
}

func sanitizeAlertConfig(cfg alert.RoutingConfig) alert.RoutingConfig {
	cfg.DefaultContactPoint = strings.TrimSpace(cfg.DefaultContactPoint)
	if cfg.ContactPoints == nil {
		cfg.ContactPoints = []alert.ContactPoint{}
	}
	if cfg.Policies == nil {
		cfg.Policies = []alert.NotificationPolicy{}
	}
	for i := range cfg.ContactPoints {
		cfg.ContactPoints[i].Name = strings.TrimSpace(cfg.ContactPoints[i].Name)
		for j := range cfg.ContactPoints[i].Integrations {
			in := &cfg.ContactPoints[i].Integrations[j]
			in.Type = strings.ToLower(strings.TrimSpace(in.Type))
			in.WebhookEnv = strings.TrimSpace(in.WebhookEnv)
			in.WebhookURL = strings.TrimSpace(in.WebhookURL)
			in.Channel = strings.TrimSpace(in.Channel)
			in.Mention = strings.TrimSpace(in.Mention)
		}
	}
	for i := range cfg.Policies {
		cfg.Policies[i].Name = strings.TrimSpace(cfg.Policies[i].Name)
		cfg.Policies[i].ContactPoint = strings.TrimSpace(cfg.Policies[i].ContactPoint)
		if cfg.Policies[i].Match == nil {
			cfg.Policies[i].Match = map[string]string{}
		}
		for k, v := range cfg.Policies[i].Match {
			delete(cfg.Policies[i].Match, k)
			key := strings.TrimSpace(k)
			if key != "" {
				cfg.Policies[i].Match[key] = strings.TrimSpace(v)
			}
		}
	}
	return cfg
}

func rejectStoredSecrets(cfg alert.RoutingConfig) error {
	for _, cp := range cfg.ContactPoints {
		for _, in := range cp.Integrations {
			if in.WebhookURL != "" {
				return fmt.Errorf("contact point %q stores a raw webhook_url; use webhook_env backed by a Kubernetes Secret instead", cp.Name)
			}
		}
	}
	return nil
}
