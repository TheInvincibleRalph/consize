package store

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open returns the configured store: Postgres when DATABASE_URL is set
// (migrations applied first when migrate is true), in-memory otherwise.
// The in-memory fallback makes every binary runnable zero-config for
// demos and tests; production runs always set DATABASE_URL.
func Open(ctx context.Context, migrate bool) (Store, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return NewMemory(), nil
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if migrate {
		if err := Migrate(ctx, pool); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return &Postgres{pool: pool}, nil
}
