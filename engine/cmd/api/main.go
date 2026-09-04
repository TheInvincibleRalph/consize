// Command api serves Consize's API: the M1 read surface (workloads,
// recommendations, savings) and the M2 safety surface (apply, the
// apply_events trail, verification runs). Without cluster access the
// apply endpoints answer 503; the read surface keeps working.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"consize/internal/api"
	"consize/internal/apply"
	"consize/internal/auth"
	"consize/internal/config"
	"consize/internal/dbapply"
	"consize/internal/pricing"
	"consize/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	var pr pricing.Service = pricing.Static{P: pricing.DefaultStatic()}
	if config.Str("CONSIZE_PRICING", "static") == "aws" {
		aws := pricing.NewAWS(config.Str("CONSIZE_REGION", "us-east-1"))
		pr = pricing.NewResilient(pricing.NewCached(aws, 24*time.Hour), pricing.DefaultStatic())
	}

	// The apply engine needs a WRITE cluster identity (docs/security.md).
	// CONSIZE_DEMO_PATCHER=true wires the DemoPatcher instead, so the
	// full safety engine (guardrails, step logic, audit trail, rollback)
	// works end-to-end without a real cluster — for the demo sandbox.
	var applier *apply.Service
	if config.Bool("CONSIZE_DEMO_PATCHER", false) {
		log.Printf("CONSIZE_DEMO_PATCHER=true — using in-memory demo patcher (no cluster required)")
		applier = apply.NewService(st, apply.NewDemoPatcher(), apply.DefaultConfig())
	} else {
		patcher, err := apply.NewK8sPatcher(config.Str("KUBECONFIG", ""))
		if err != nil {
			log.Printf("no cluster write identity (%v) — apply endpoints disabled (503)", err)
		} else {
			applier = apply.NewService(st, patcher, apply.DefaultConfig())
		}
	}

	// Database class changes route to the DB engine. The provider seam
	// is a stub until a live cloud integration lands (ADR-030): dry-runs
	// and guardrail evaluation work end to end, real class changes return
	// the stub's explicit "manual class change required" error.
	dbApplier := dbapply.NewService(st, dbapply.StubChanger{}, dbapply.DefaultConfig())

	// Authentication (ADR-037). CONSIZE_AUTH_REQUIRED defaults to true —
	// the enterprise posture — and the demo (embedded SPA fallback, curl
	// actor flows) opts out with CONSIZE_AUTH_REQUIRED=false. CONSIZE_
	// BOOTSTRAP_ADMIN="email:password" creates the first admin once, only
	// while the users table is empty (see CreateBootstrapAdmin).
	var opts api.Options
	authRequired := config.Bool("CONSIZE_AUTH_REQUIRED", true)
	if authRequired {
		authSvc := auth.NewService(st)
		if b := config.Str("CONSIZE_BOOTSTRAP_ADMIN", ""); b != "" {
			email, password, ok := strings.Cut(b, ":")
			if !ok || email == "" || password == "" {
				log.Fatalf("CONSIZE_BOOTSTRAP_ADMIN must be \"email:password\"")
			}
			created, err := authSvc.CreateBootstrapAdmin(ctx, email, password)
			if err != nil {
				log.Fatalf("bootstrap admin: %v", err)
			}
			if created {
				log.Printf("created bootstrap admin %s", email)
			}
		}
		opts = api.Options{
			Auth:         authSvc,
			CookieSecure: config.Bool("CONSIZE_COOKIE_SECURE", false),
		}
	} else {
		log.Printf("CONSIZE_AUTH_REQUIRED=false — authentication disabled (demo build)")
	}

	srv := &http.Server{
		Addr:              ":" + config.Str("CONSIZE_LISTEN_PORT", "8080"),
		Handler:           api.New(st, pr, applier, dbApplier, opts),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("consize api listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
