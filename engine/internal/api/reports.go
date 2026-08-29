package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"consize/internal/alert"
	"consize/internal/report"
)

type reportConfigResponse struct {
	Config report.Config `json:"config"`
	Source string        `json:"source"`
}

func (s *Server) getReportConfig(w http.ResponseWriter, r *http.Request) {
	cfg, source, err := s.reportConfig(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reportConfigResponse{Config: cfg, Source: source})
}

func (s *Server) putReportConfig(w http.ResponseWriter, r *http.Request) {
	var cfg report.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be a report config"})
		return
	}
	cfg, err := report.NormalizeConfig(cfg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.store.PutSetting(r.Context(), report.StoreConfigKey, string(raw)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reportConfigResponse{Config: cfg, Source: "store"})
}

func (s *Server) getSavingsReport(w http.ResponseWriter, r *http.Request) {
	rangeDays, err := s.reportRangeDays(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	summary, err := report.Build(r.Context(), s.store, time.Now().UTC(), rangeDays)
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "pdf") {
		b, err := report.PDF(summary)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := fmt.Sprintf("consize-savings-report-%dd-%s.pdf", rangeDays, summary.To.Format("2006-01-02"))
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": summary})
}

func (s *Server) sendSavingsReport(w http.ResponseWriter, r *http.Request) {
	rangeDays, err := s.reportRangeDays(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	summary, err := report.Build(r.Context(), s.store, time.Now().UTC(), rangeDays)
	if err != nil {
		writeErr(w, err)
		return
	}
	alertCfg, _, err := s.alertingConfig(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	results := alert.NewWithConfig(alertCfg).DeliverEvent(r.Context(), report.Event(summary))
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
	writeJSON(w, code, map[string]any{"report": summary, "results": results})
}

func (s *Server) reportConfig(r *http.Request) (report.Config, string, error) {
	if raw, ok, err := s.store.GetSetting(r.Context(), report.StoreConfigKey); err != nil {
		return report.Config{}, "", err
	} else if ok && strings.TrimSpace(raw) != "" {
		cfg, err := report.ParseConfig(raw)
		if err != nil {
			return report.Config{}, "", fmt.Errorf("stored report config: %w", err)
		}
		return cfg, "store", nil
	}
	return report.DefaultConfig(), "default", nil
}

func (s *Server) reportRangeDays(r *http.Request) (int, error) {
	if v := r.URL.Query().Get("range"); v != "" {
		return parseReportRange(v)
	}
	if v := r.URL.Query().Get("range_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("range_days must be one of 7, 14, or 30")
		}
		return parseReportRange(strconv.Itoa(n) + "d")
	}
	var body struct {
		RangeDays int `json:"range_days"`
	}
	if r.Body != nil && r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.RangeDays != 0 {
		return parseReportRange(strconv.Itoa(body.RangeDays) + "d")
	}
	cfg, _, err := s.reportConfig(r)
	if err != nil {
		return 0, err
	}
	return cfg.RangeDays, nil
}

func parseReportRange(v string) (int, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.TrimSuffix(v, "days")
	v = strings.TrimSuffix(v, "day")
	v = strings.TrimSuffix(v, "d")
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("range must be one of 7d, 14d, or 30d")
	}
	_, err = report.NormalizeConfig(report.Config{RangeDays: n})
	if err != nil {
		return 0, err
	}
	return n, nil
}
