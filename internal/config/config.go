package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds runtime configuration loaded from environment variables.
type Config struct {
	Port            string
	DatabaseURL     string
	Schema          string
	MaxConns        int32
	QueryTimeoutMS  int
	CORSAllowOrigin string
}

func Load() (*Config, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required (pooled Supavisor connection string)")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	schema := os.Getenv("DB_SCHEMA")
	if schema == "" {
		schema = "hostiggo_testing_schema"
	}

	maxConns := int32(10)
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid DB_MAX_CONNS: %w", err)
		}
		maxConns = int32(n)
	}

	timeout := 10000
	if v := os.Getenv("QUERY_TIMEOUT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid QUERY_TIMEOUT_MS: %w", err)
		}
		timeout = n
	}

	// This API is fully public/anonymous (no auth header, no cookies — same
	// as the original Supabase anon-key search path), so a permissive
	// default is consistent with that, not a widening of access. Set to a
	// specific origin (or a comma-separated list is NOT supported here —
	// use a single origin, or "*") once the frontend's real domain is known.
	corsOrigin := os.Getenv("CORS_ALLOW_ORIGIN")
	if corsOrigin == "" {
		corsOrigin = "*"
	}

	return &Config{
		Port:            port,
		DatabaseURL:     dbURL,
		Schema:          schema,
		MaxConns:        maxConns,
		QueryTimeoutMS:  timeout,
		CORSAllowOrigin: corsOrigin,
	}, nil
}
