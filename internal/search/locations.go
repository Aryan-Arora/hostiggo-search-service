package search

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type LocationRow struct {
	LocationID        int64   `json:"location_id"`
	State             string  `json:"state"`
	District          string  `json:"district"`
	LowerDivisionName *string `json:"lower_division_name"`
}

type LocationService struct {
	pool *pgxpool.Pool
}

func NewLocationService(pool *pgxpool.Pool) *LocationService {
	return &LocationService{pool: pool}
}

const defaultLocationLimit = 22

func clampLocationLimit(n int) int {
	if n < 1 {
		return defaultLocationLimit
	}
	if n > 100 {
		return 100
	}
	return n
}

// Search replaces search_locations_partial. It uses the same lexical
// full-text approach as listing search (websearch_to_tsquery against a
// persisted, GIN-indexed search_vector column on locations) rather than the
// original function's regex-built to_tsquery string, and rather than any
// fuzzy/trigram matching.
func (s *LocationService) Search(ctx context.Context, q string, limit int) ([]LocationRow, error) {
	limit = clampLocationLimit(limit)
	q = strings.TrimSpace(q)
	if q == "" {
		return s.Sample(ctx, limit)
	}

	rows, err := s.pool.Query(ctx, `
SELECT location_id, state, district, lower_division_name
FROM locations
WHERE search_vector @@ websearch_to_tsquery('english', $1)
ORDER BY location_id
LIMIT $2
`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("location search query: %w", err)
	}
	defer rows.Close()

	return scanLocationRows(rows)
}

// Sample backs GET /api/locations without `q` (plain, uncached-by-this-service
// list; caching is an HTTP-layer concern for the caller/CDN).
func (s *LocationService) Sample(ctx context.Context, limit int) ([]LocationRow, error) {
	limit = clampLocationLimit(limit)
	rows, err := s.pool.Query(ctx, `
SELECT location_id, state, district, lower_division_name
FROM locations
ORDER BY location_id
LIMIT $1
`, limit)
	if err != nil {
		return nil, fmt.Errorf("location sample query: %w", err)
	}
	defer rows.Close()
	return scanLocationRows(rows)
}

// Popular backs GET /api/locations?popular=1: locations ranked by active
// listing count.
func (s *LocationService) Popular(ctx context.Context, limit int) ([]LocationRow, error) {
	limit = clampLocationLimit(limit)
	rows, err := s.pool.Query(ctx, `
SELECT loc.location_id, loc.state, loc.district, loc.lower_division_name
FROM locations loc
JOIN listings l ON l.location_id = loc.location_id AND l.is_active = TRUE
GROUP BY loc.location_id, loc.state, loc.district, loc.lower_division_name
ORDER BY count(l.listing_id) DESC
LIMIT $1
`, limit)
	if err != nil {
		return nil, fmt.Errorf("popular locations query: %w", err)
	}
	defer rows.Close()
	return scanLocationRows(rows)
}

func scanLocationRows(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}) ([]LocationRow, error) {
	out := []LocationRow{}
	for rows.Next() {
		var r LocationRow
		if err := rows.Scan(&r.LocationID, &r.State, &r.District, &r.LowerDivisionName); err != nil {
			return nil, fmt.Errorf("scanning location row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
