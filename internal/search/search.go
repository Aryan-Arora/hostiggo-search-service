// Package search implements the listing search endpoint against a direct
// Postgres connection, porting the behavior of the "complete spec" RPC
// (hostiggo_testing_schema.search_listings / listing_search_view) rather
// than the live-but-partial search_listings_by_state path.
//
// Notably this does NOT use fuzzy/similarity matching anywhere (no pg_trgm,
// no edit-distance). Free-text relevance uses Postgres full-text search
// (tsvector/tsquery) against a persisted, GIN-indexed search_vector column
// added in migrations/0001_add_search_vectors.sql.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

const (
	defaultPageSize = 50
	maxPageSize     = 100
	minPageSize     = 1
)

// Service executes listing search queries.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// clampPageSize mirrors the original route's
// Math.min(100, Math.max(1, pageSize ?? 50)).
func clampPageSize(p *int) int {
	if p == nil {
		return defaultPageSize
	}
	n := *p
	if n < minPageSize {
		n = minPageSize
	}
	if n > maxPageSize {
		n = maxPageSize
	}
	return n
}

// whereBuilder accumulates SQL predicates and their positional args so the
// same WHERE clause can be reused, verbatim, for both the page query and the
// COUNT(*) query. The original schema's own migration comments flag WHERE
// clause drift between search_listings_by_state and its _count sibling as a
// real bug risk; sharing one builder here removes that risk by construction.
type whereBuilder struct {
	conds []string
	args  []interface{}
}

func (w *whereBuilder) add(cond string, args ...interface{}) {
	// Renumber $N placeholders in cond to match the running arg offset, in a
	// single scan (sequential ReplaceAll calls would corrupt e.g. $11 once
	// the offset pushes double-digit indices, since "$1" is a substring of
	// "$11").
	base := len(w.args)
	cond = placeholderRe.ReplaceAllStringFunc(cond, func(m string) string {
		n, _ := strconv.Atoi(m[1:])
		return "$" + strconv.Itoa(base+n)
	})
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *whereBuilder) clause() string {
	if len(w.conds) == 0 {
		return "TRUE"
	}
	return strings.Join(w.conds, "\n    AND ")
}

func buildFilters(f *Filters) *whereBuilder {
	w := &whereBuilder{}
	w.add("l.is_active = TRUE")

	if f == nil {
		return w
	}

	if f.State != nil && *f.State != "" {
		w.add("LOWER(loc.state) = LOWER($1)", *f.State)
	}
	if f.District != nil && *f.District != "" {
		w.add("LOWER(loc.district) = LOWER($1)", *f.District)
	}
	if f.MinPrice != nil {
		w.add("l.price_weekday >= $1", *f.MinPrice)
	}
	if f.MaxPrice != nil {
		w.add("l.price_weekday <= $1", *f.MaxPrice)
	}
	if f.TotalGuests != nil {
		w.add("l.num_guests >= $1", *f.TotalGuests)
	}
	if len(f.Amenities) > 0 {
		w.add(`EXISTS (
      SELECT 1 FROM listing_amenities la2
      WHERE la2.listing_id = l.listing_id AND la2.amenity_id = ANY($1)
    )`, f.Amenities)
	}
	// roomTypes/propertyTypes: the original client/route code disagreed on
	// which key name carried this value (see audit §3); this service accepts
	// either and treats them as the same filter (property type name).
	roomTypeNames := f.RoomTypes
	if len(f.PropertyTypes) > 0 {
		roomTypeNames = f.PropertyTypes
	}
	if len(roomTypeNames) > 0 {
		w.add("pt.name = ANY($1)", roomTypeNames)
	}
	if len(f.StayTypes) > 0 {
		w.add("st.title = ANY($1)", f.StayTypes)
	}
	if len(f.Ratings) > 0 {
		w.add("FLOOR(COALESCE(rv.avg_rating, 0))::int = ANY($1)", f.Ratings)
	}
	if f.StartDate != nil && f.EndDate != nil && *f.StartDate != "" && *f.EndDate != "" {
		// Availability check: deliberately reads only the minimal
		// listing_id/date/status columns needed to exclude unavailable
		// listings from public search results. bookings has row-level
		// security scoping SELECT to the owning guest/host; this direct
		// connection has no anon JWT to scope by, so RLS can't apply here.
		// This query is the explicit, public-safe replacement for that
		// policy: it never selects guest/host identifying columns, only
		// enough to compute "is this listing free in this date range".
		w.add(`NOT EXISTS (
      SELECT 1 FROM bookings b
      WHERE b.listing_id = l.listing_id
        AND b.status_id != 3
        AND b.start_date < $2::date
        AND b.end_date > $1::date
    )`, *f.StartDate, *f.EndDate)
		w.add(`NOT EXISTS (
      SELECT 1 FROM listing_calendar lc
      WHERE lc.listing_id = l.listing_id
        AND lc.is_available = FALSE
        AND lc.date >= $1::date
        AND lc.date < $2::date
    )`, *f.StartDate, *f.EndDate)
	}
	if f.Query != nil && strings.TrimSpace(*f.Query) != "" {
		// Lexical full-text search only: websearch_to_tsquery tokenizes and
		// stems the input the same way the indexed search_vector column
		// does. No similarity/trigram/edit-distance matching is used.
		w.add("l.search_vector @@ websearch_to_tsquery('english', $1)", *f.Query)
	}

	return w
}

func hasGeo(f *Filters) bool {
	return f != nil && f.Latitude != nil && f.Longitude != nil
}

func hasQuery(f *Filters) bool {
	return f != nil && f.Query != nil && strings.TrimSpace(*f.Query) != ""
}

// isRanked reports whether this request sorts by relevance/distance rather
// than by listing_id. Ranked queries paginate by row offset (still returned
// to the client as the opaque `cursor` field); id-sorted queries paginate by
// listing_id > cursor, exactly like the original search_listings_by_state.
func isRanked(f *Filters) bool {
	return hasQuery(f) || hasGeo(f)
}

// Search executes the listing query and returns a response shaped exactly
// like the original POST /api/search endpoint's response.
func (s *Service) Search(ctx context.Context, req Request) (*Response, error) {
	pageSize := clampPageSize(req.PageSize)
	filters := req.Filters
	ranked := isRanked(filters)

	w := buildFilters(filters)
	whereClause := w.clause()
	args := append([]interface{}{}, w.args...)

	var cursorOffset int64
	if req.Cursor != nil {
		cursorOffset = *req.Cursor
	}

	// id-mode pagination: listing_id > cursor, keeping exact parity with the
	// live path's cursor semantics when there's no relevance/geo ordering.
	if !ranked && req.Cursor != nil {
		args = append(args, *req.Cursor)
		whereClause = whereClause + fmt.Sprintf("\n    AND l.listing_id > $%d", len(args))
	}

	distanceExpr := "NULL::float8"
	orderExpr := "l.listing_id ASC"
	if hasGeo(filters) {
		args = append(args, *filters.Longitude, *filters.Latitude)
		lonIdx := len(args) - 1
		latIdx := len(args)
		distanceExpr = fmt.Sprintf(
			`ST_Distance(
        ST_SetSRID(ST_MakePoint(l.longitude, l.latitude), 4326)::geography,
        ST_SetSRID(ST_MakePoint($%d, $%d), 4326)::geography
      )`, lonIdx, latIdx)
		orderExpr = distanceExpr + " ASC, l.listing_id ASC"
	} else if hasQuery(filters) {
		args = append(args, *filters.Query)
		qIdx := len(args)
		orderExpr = fmt.Sprintf(
			"ts_rank(l.search_vector, websearch_to_tsquery('english', $%d)) DESC, l.listing_id ASC", qIdx)
	}

	limitArgIdx := len(args) + 1
	args = append(args, pageSize+1) // fetch one extra row to compute hasMore

	offsetClause := ""
	if ranked && cursorOffset > 0 {
		offsetArgIdx := len(args) + 1
		args = append(args, cursorOffset)
		offsetClause = fmt.Sprintf("OFFSET $%d", offsetArgIdx)
	}

	query := fmt.Sprintf(`
SELECT
  l.listing_id, l.title, l.description, l.price_weekday, l.price_weekend,
  l.num_guests, l.num_bedrooms, l.num_beds, l.num_bathrooms,
  l.latitude, l.longitude, l.property_type_id, l.stay_type_id, l.location_id,
  loc.state, loc.district,
  pt.name AS property_type_name, st.title AS stay_type_title,
  COALESCE(rv.avg_rating, 0) AS avg_rating,
  COALESCE(rv.review_count, 0) AS review_count,
  COALESCE(med.media, '[]'::jsonb) AS listing_media,
  COALESCE(am.amenities, '[]'::jsonb) AS listing_amenities,
  %s AS distance
FROM listings l
LEFT JOIN locations loc ON l.location_id = loc.location_id
LEFT JOIN property_types pt ON l.property_type_id = pt.id
LEFT JOIN stay_types st ON l.stay_type_id = st.id
LEFT JOIN LATERAL (
  SELECT jsonb_agg(jsonb_build_object('media_url', m.media_url, 'is_cover', m.is_cover)) AS media
  FROM listing_media m WHERE m.listing_id = l.listing_id
) med ON TRUE
LEFT JOIN LATERAL (
  SELECT jsonb_agg(jsonb_build_object('amenities', jsonb_build_object('name', a.name))) AS amenities
  FROM listing_amenities la JOIN amenities a ON la.amenity_id = a.amenity_id
  WHERE la.listing_id = l.listing_id
) am ON TRUE
LEFT JOIN LATERAL (
  SELECT avg(r.rating) AS avg_rating, count(r.rating) AS review_count
  FROM review r WHERE r.listing_id = l.listing_id
) rv ON TRUE
WHERE %s
ORDER BY %s
LIMIT $%d
%s
`, distanceExpr, whereClause, orderExpr, limitArgIdx, offsetClause)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	results := make([]Result, 0, pageSize)
	for rows.Next() {
		var (
			r             Result
			mediaJSON     []byte
			amenitiesJSON []byte
		)
		l := &r.Listing
		if err := rows.Scan(
			&l.ListingID, &l.Title, &l.Description, &l.PriceWeekday, &l.PriceWeekend,
			&l.NumGuests, &l.NumBedrooms, &l.NumBeds, &l.NumBathrooms,
			&l.Latitude, &l.Longitude, &l.PropertyTypeID, &l.StayTypeID, &l.LocationID,
			&l.Locations.State, &l.Locations.District,
			&l.PropertyTypeName, &l.StayTypeTitle,
			&l.AvgRating, &l.ReviewCount,
			&mediaJSON, &amenitiesJSON,
			&r.Distance,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		if err := json.Unmarshal(mediaJSON, &l.ListingMedia); err != nil {
			return nil, fmt.Errorf("decoding listing_media: %w", err)
		}
		if err := json.Unmarshal(amenitiesJSON, &l.ListingAmenities); err != nil {
			return nil, fmt.Errorf("decoding listing_amenities: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	hasMore := len(results) > pageSize
	if hasMore {
		results = results[:pageSize]
	}

	resp := &Response{
		Data:    results,
		HasMore: hasMore,
	}

	if len(results) > 0 {
		if ranked {
			nextCursor := cursorOffset + int64(len(results))
			resp.Cursor = &nextCursor
		} else {
			lastID := results[len(results)-1].Listing.ListingID
			resp.Cursor = &lastID
		}
	}

	// totalCount is only computed on the first page, mirroring the original
	// route (it ran a second no-LIMIT RPC for this; here we reuse the exact
	// same WHERE clause/args built above, so it can't drift from the page
	// query's predicates).
	if req.Cursor == nil {
		countQuery := fmt.Sprintf(`
SELECT count(*)
FROM listings l
LEFT JOIN locations loc ON l.location_id = loc.location_id
LEFT JOIN property_types pt ON l.property_type_id = pt.id
LEFT JOIN stay_types st ON l.stay_type_id = st.id
LEFT JOIN LATERAL (
  SELECT avg(r.rating) AS avg_rating FROM review r WHERE r.listing_id = l.listing_id
) rv ON TRUE
WHERE %s
`, w.clause())
		var total int64
		if err := s.pool.QueryRow(ctx, countQuery, w.args...).Scan(&total); err != nil {
			return nil, fmt.Errorf("count query: %w", err)
		}
		resp.TotalCount = &total
	}

	return resp, nil
}
