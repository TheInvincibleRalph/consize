// tmp-localdemo seeds the local poke database: k8s surface stubbed
// (empty), DB surface from the fixture — then the API serves it.
package main

import (
	"context"
	"log"
	"time"

	"consize/internal/collector"
	"consize/internal/dbmetrics"
	"consize/internal/store"
)

type stubMeta struct{}

func (stubMeta) ListDeployments(context.Context) ([]collector.DeploymentInfo, error) { return nil, nil }
func (stubMeta) PodOwners(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

type stubProm struct{}

func (stubProm) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]collector.Series, error) {
	return nil, nil
}

func main() {
	ctx := context.Background()
	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	c := collector.New(stubMeta{}, stubProm{}, st, 15*time.Minute, 14*24*time.Hour)
	c.DB = dbmetrics.NewFixture()
	if err := c.Run(ctx); err != nil {
		log.Fatalf("collect: %v", err)
	}
	log.Println("local demo seeded")
}
