package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aryan-Arora/hostiggo-search-service/internal/config"
)

// New creates a pooled pgx connection to Postgres (Supabase Supavisor pooled
// connection string), targeting the given schema via search_path.
func New(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	// Every session on this connection resolves unqualified table names
	// against the app's schema first, matching the Supabase client's
	// db.schema pin, then falls back to public for extensions (postgis, etc).
	poolCfg.ConnConfig.RuntimeParams["search_path"] = fmt.Sprintf("%s,public", cfg.Schema)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return pool, nil
}
