package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aryan-Arora/search-service-backend/internal/config"
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

	// Supabase's pooled connection strings (Supavisor, port 6543) run in
	// transaction mode: the physical connection is handed to a different
	// logical session between transactions. pgx's default query mode caches
	// server-side prepared statements by name on the connection, which then
	// collides with a statement of the same name left behind by whichever
	// other session used that connection previously ("prepared statement
	// ... already exists", SQLSTATE 42P05). Simple protocol mode never
	// prepares statements server-side, which is what Supabase's own docs
	// recommend for pgx behind Supavisor transaction pooling.
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

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
