// Command costscan discovers unmanaged cloud-cost opportunities.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"consize/internal/config"
	"consize/internal/costscan"
	"consize/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	src, via, err := costscan.SourceFor(config.Str("CONSIZE_COSTSCAN", "none"))
	if err != nil {
		log.Fatalf("CONSIZE_COSTSCAN: %v", err)
	}
	if via != "" {
		log.Printf("costscan: source %s", via)
	}

	opps, err := costscan.Service{Source: src, Store: st}.Run(ctx)
	if err != nil {
		log.Fatalf("costscan: %v", err)
	}
	var total float64
	for _, o := range opps {
		total += o.MonthlyCost
		log.Printf("costscan: %s %s/%s %s $%.2f/mo",
			o.ResourceType, o.Region, o.Name, o.Action, o.MonthlyCost)
	}
	log.Printf("costscan: %d open opportunities, $%.2f/mo", len(opps), total)
}
