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

// buildFilters builds the WHERE clause for the primary (exact) match. Pass
// includeDistrict=false to build the same filter set but with the district
// predicate omitted entirely — used by the radius-expansion fallback
// (expand.go) to search the surrounding area without also re-matching the
// district that fallback exists to expand beyond.
func buildFilters(f *Filters, includeDistrict bool) *whereBuilder {
	w := &whereBuilder{}
	w.add("l.is_active = TRUE")

	if f == nil {
		return w
	}

	if f.State != nil && *f.State != "" {
		w.add("LOWER(loc.state) = LOWER($1)", *f.State)
	}
	if includeDistrict && f.District != nil && *f.District != "" {
		// The `district` field doubles as a free-text "destination" box on
		// the frontend, so it sometimes actually holds a STATE name (e.g.
		// someone types "Uttarakhand" rather than "Dehradun"). No district
		// is ever literally named after its own state, so a pure district
		// match returns nothing — matching real data confirmed this exact
		// failure. Rather than a curated per-state alias list (which would
		// need updating by hand for every future state), match on EITHER
		// the district (including its known aliases, e.g. "New Delhi" <->
		// "Delhi" — see synonyms.go) OR the state name directly, live
		// against whatever states/districts actually exist — so this
		// generalizes to every current state and any added later without
		// code changes.
		districtInput := strings.ToLower(strings.TrimSpace(*f.District))
		w.add("(LOWER(loc.district) = ANY($1) OR LOWER(loc.state) = $2)",
			expandDistrictAliases(*f.District), districtInput)
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

// selectCols is the column list shared by every ordering mode's final
// SELECT, keeping the row-scanning code in Search() identical regardless of
// which mode built the query.
const selectCols = `
  l.listing_id, l.title, l.description, l.price_weekday, l.price_weekend,
  l.num_guests, l.num_bedrooms, l.num_beds, l.num_bathrooms,
  l.latitude, l.longitude, l.property_type_id, l.stay_type_id, l.location_id,
  loc.state, loc.district,
  pt.name AS property_type_name, st.title AS stay_type_title,
  COALESCE(rv.avg_rating, 0) AS avg_rating,
  COALESCE(rv.review_count, 0) AS review_count,
  COALESCE(med.media, '[]'::jsonb) AS listing_media,
  COALESCE(am.amenities, '[]'::jsonb) AS listing_amenities`

// joinBlock is the join structure shared by every ordering mode.
const joinBlock = `
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
) rv ON TRUE`

// buildQuery constructs the page query and its full positional-arg list for
// one of three ordering modes. All three paginate by row offset (not
// listing_id), since none of them produce a listing_id-monotonic order:
//   - geo:  ORDER BY distance to the given point
//   - text: ORDER BY ts_rank against the free-text query
//   - default (neither of the above): a diversified price-tier interleave —
//     see buildDefaultOrderQuery's own comment for why and how.
func buildQuery(w *whereBuilder, filters *Filters, pageSize int, cursorOffset int64) (string, []interface{}) {
	args := append([]interface{}{}, w.args...)

	switch {
	case hasGeo(filters):
		args = append(args, *filters.Longitude, *filters.Latitude)
		lonIdx := len(args) - 1
		latIdx := len(args)
		distanceExpr := fmt.Sprintf(
			`ST_Distance(
        ST_SetSRID(ST_MakePoint(l.longitude, l.latitude), 4326)::geography,
        ST_SetSRID(ST_MakePoint($%d, $%d), 4326)::geography
      )`, lonIdx, latIdx)
		orderExpr := distanceExpr + " ASC, l.listing_id ASC"
		limitIdx := len(args) + 1
		args = append(args, pageSize+1)
		offsetIdx := len(args) + 1
		args = append(args, cursorOffset)
		query := fmt.Sprintf(`
SELECT%s,
  %s AS distance
%s
WHERE %s
ORDER BY %s
LIMIT $%d OFFSET $%d
`, selectCols, distanceExpr, joinBlock, w.clause(), orderExpr, limitIdx, offsetIdx)
		return query, args

	case hasQuery(filters):
		args = append(args, *filters.Query)
		qIdx := len(args)
		orderExpr := fmt.Sprintf(
			"ts_rank(l.search_vector, websearch_to_tsquery('english', $%d)) DESC, l.listing_id ASC", qIdx)
		limitIdx := len(args) + 1
		args = append(args, pageSize+1)
		offsetIdx := len(args) + 1
		args = append(args, cursorOffset)
		query := fmt.Sprintf(`
SELECT%s,
  NULL::float8 AS distance
%s
WHERE %s
ORDER BY %s
LIMIT $%d OFFSET $%d
`, selectCols, joinBlock, w.clause(), orderExpr, limitIdx, offsetIdx)
		return query, args

	default:
		limitIdx := len(args) + 1
		args = append(args, pageSize+1)
		offsetIdx := len(args) + 1
		args = append(args, cursorOffset)
		query := fmt.Sprintf(`
WITH filtered AS (
  SELECT%s
  %s
  WHERE %s
),
tiered AS (
  SELECT *, NTILE(3) OVER (ORDER BY price_weekday NULLS LAST, listing_id) AS _tier
  FROM filtered
),
positioned AS (
  SELECT *, ROW_NUMBER() OVER (PARTITION BY _tier ORDER BY listing_id) - 1 AS _r
  FROM tiered
)
SELECT
  listing_id, title, description, price_weekday, price_weekend,
  num_guests, num_bedrooms, num_beds, num_bathrooms,
  latitude, longitude, property_type_id, stay_type_id, location_id,
  state, district, property_type_name, stay_type_title,
  avg_rating, review_count, listing_media, listing_amenities,
  NULL::float8 AS distance
FROM positioned
ORDER BY
  -- Default browse order: mid, low, mid, low, high, repeating. Price tiers
  -- (_tier: 1=low, 2=mid, 3=high) are Postgres NTILE(3) tertiles of
  -- whatever this request's own filtered result set actually contains, not
  -- a fixed rupee cutoff — so "mid" always means "middle third of these
  -- results," in Delhi or in a 2-listing town alike. Within a 5-position
  -- block, mid supplies positions 0 and 2, low supplies 1 and 3, high
  -- supplies 4 — so mid/low are each consumed 2-per-block and high 1-per-
  -- block; _r (each tier's own 0-indexed row number) maps directly to a
  -- block index and a within-block slot via integer division/modulo. This
  -- ONLY applies with no free-text q and no lat/lon geo-sort — either of
  -- those takes over ordering entirely (see the other two branches above).
  CASE _tier
    WHEN 2 THEN (_r / 2) * 5 + (CASE WHEN _r %% 2 = 0 THEN 0 ELSE 2 END)
    WHEN 1 THEN (_r / 2) * 5 + (CASE WHEN _r %% 2 = 0 THEN 1 ELSE 3 END)
    WHEN 3 THEN _r * 5 + 4
  END
LIMIT $%d OFFSET $%d
`, selectCols, joinBlock, w.clause(), limitIdx, offsetIdx)
		return query, args
	}
}

// runListingQuery executes a query built by buildQuery (or the expansion
// path's equivalent) and scans it into Results. Shared so the normal path
// and the radius-expansion path (expand.go) can't drift in how they read
// the same column shape.
func (s *Service) runListingQuery(ctx context.Context, query string, args []interface{}, pageSize int) ([]Result, error) {
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
	return results, nil
}

// Search executes the listing query and returns a response shaped exactly
// like the original POST /api/search endpoint's response.
func (s *Service) Search(ctx context.Context, req Request) (*Response, error) {
	pageSize := clampPageSize(req.PageSize)
	filters := req.Filters

	w := buildFilters(filters, true)

	var cursorOffset int64
	if req.Cursor != nil {
		cursorOffset = *req.Cursor
	}

	// Radius-expansion fallback: only when a district was actually searched
	// for, and only for the default ordering mode (see expand.go's doc
	// comment for why q/geo-sort are out of scope for now).
	if filters != nil && filters.District != nil && *filters.District != "" &&
		!hasGeo(filters) && !hasQuery(filters) {
		plan, err := s.planExpansion(ctx, w, filters, pageSize)
		if err != nil {
			return nil, fmt.Errorf("planning radius expansion: %w", err)
		}
		if plan != nil {
			return s.searchWithExpansion(ctx, w, filters, plan, pageSize, cursorOffset)
		}
	}

	query, args := buildQuery(w, filters, pageSize, cursorOffset)

	results, err := s.runListingQuery(ctx, query, args, pageSize)
	if err != nil {
		return nil, err
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
		// Every mode now paginates by row offset (see buildQuery's doc
		// comment) — cursor is an opaque running offset, not a listing_id.
		nextCursor := cursorOffset + int64(len(results))
		resp.Cursor = &nextCursor
	}

	// totalCount is only computed on the first page, mirroring the original
	// route (it ran a second no-LIMIT RPC for this; here we reuse the exact
	// same WHERE clause/args built above, so it can't drift from the page
	// query's predicates).
	if req.Cursor == nil {
		total, err := s.countMatching(ctx, w)
		if err != nil {
			return nil, err
		}
		resp.TotalCount = &total
	}

	return resp, nil
}

// countMatching runs COUNT(*) for a WHERE clause built by buildFilters. The
// join set here only needs to support whatever a filter might reference
// (rv.avg_rating for the ratings filter) — it doesn't need media/amenities
// aggregation, unlike the page query.
func (s *Service) countMatching(ctx context.Context, w *whereBuilder) (int64, error) {
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
		return 0, fmt.Errorf("count query: %w", err)
	}
	return total, nil
}
