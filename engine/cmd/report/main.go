package main

import (
	"context"
	"log"
	"time"

	"consize/internal/alert"
	"consize/internal/report"
	"consize/internal/store"
)

func main() {
	ctx := context.Background()

	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	if pg, ok := st.(*store.Postgres); ok {
		defer pg.Close()
	}

	cfg := report.DefaultConfig()
	if raw, ok, err := st.GetSetting(ctx, report.StoreConfigKey); err != nil {
		log.Fatalf("report config: %v", err)
	} else if ok {
		cfg, err = report.ParseConfig(raw)
		if err != nil {
			log.Fatalf("report config: %v", err)
		}
	}
	if !cfg.Enabled {
		log.Printf("weekly report disabled")
		return
	}

	summary, err := report.Build(ctx, st, time.Now().UTC(), cfg.RangeDays)
	if err != nil {
		log.Fatalf("build report: %v", err)
	}
	results := alert.NewWithConfigProvider(st).DeliverEvent(ctx, report.Event(summary))
	if len(results) == 0 {
		log.Fatalf("send report: no alerting contact point matched")
	}
	failed := 0
	for _, result := range results {
		if result.Status == "failed" {
			failed++
			log.Printf("send report failed: contact_point=%s type=%s error=%s", result.ContactPoint, result.Type, result.Error)
		}
	}
	if failed > 0 {
		log.Fatalf("send report: %d delivery failure(s)", failed)
	}
	log.Printf("weekly report sent: range_days=%d deliveries=%d", cfg.RangeDays, len(results))
}
