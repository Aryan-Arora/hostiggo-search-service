// Radius-expansion fallback: when a searched district doesn't have enough
// listings to fill a page, top up the remainder with nearby listings found
// by real geographic distance (PostGIS), rather than leaving the page
// short. Example this was built for: "New Delhi" has zero listings of its
// own in this dataset — searching it should still surface nearby Gurgaon/
// Delhi listings instead of an empty page.
//
// Scope: only engages for the default ordering mode with a `district`
// filter present (not `q` free-text, not lat/lon geo-sort — either of those
// is already a more specific request the user made on purpose, and
// layering a second, different distance concept on top of an explicit one
// would be surprising). See buildExpandedQuery for how the two result
// groups (exact-district matches, then nearby fallback matches) are
// combined into one ordered, paginated set.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// radiiMeters is the widening ladder: try the smallest radius first, and
// only reach for a bigger one if the district + everything within the
// smaller radius still can't fill a page. Capped at 200km so a genuinely
// sparse region doesn't silently pull in results from halfway across the
// country.
var radiiMeters = []float64{15_000, 30_000, 60_000, 120_000, 200_000}

type expansionPlan struct {
	primaryTotal  int64
	combinedTotal int64
	radiusMeters  float64
	refLat        float64
	refLon        float64
}

// planExpansion decides whether radius expansion is needed and, if so, how
// far it needs to reach. Returns (nil, nil) when expansion isn't needed or
// isn't possible (e.g. no reference point can be derived at all) — the
// caller falls back to the normal single-district query in that case.
func (s *Service) planExpansion(ctx context.Context, w *whereBuilder, filters *Filters, pageSize int) (*expansionPlan, error) {
	primaryTotal, err := s.countMatching(ctx, w)
	if err != nil {
		return nil, err
	}
	if primaryTotal >= int64(pageSize) {
		return nil, nil
	}

	refLat, refLon, ok, err := s.referencePoint(ctx, w, filters)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	wExpanded := buildExpansionWhereBuilder(filters)
	buckets, err := s.expandedBucketCounts(ctx, wExpanded, refLat, refLon)
	if err != nil {
		return nil, err
	}

	remaining := int64(pageSize) - primaryTotal
	var cumulative int64
	chosenIdx := len(radiiMeters) - 1 // default: reach the cap if never satisfied
	for i, c := range buckets {
		cumulative += c
		if cumulative >= remaining {
			chosenIdx = i
			break
		}
	}
	// Recompute cumulative up through chosenIdx specifically (the loop above
	// may have stopped early or run to the end without breaking).
	var combinedExtra int64
	for i := 0; i <= chosenIdx; i++ {
		combinedExtra += buckets[i]
	}

	return &expansionPlan{
		primaryTotal:  primaryTotal,
		combinedTotal: primaryTotal + combinedExtra,
		radiusMeters:  radiiMeters[chosenIdx],
		refLat:        refLat,
		refLon:        refLon,
	}, nil
}

// buildExpansionWhereBuilder builds the WHERE clause for the fallback pool:
// every filter except district, plus an explicit exclusion of the searched
// district's own alias group (so a listing never appears in both the
// primary and fallback groups).
func buildExpansionWhereBuilder(filters *Filters) *whereBuilder {
	w := buildFilters(filters, false)
	if filters != nil && filters.District != nil && *filters.District != "" {
		// Mirrors buildFilters' primary-match predicate exactly (district
		// alias OR state name) — otherwise a listing matched into the
		// primary group via the state-name path could also leak into the
		// fallback group as a duplicate.
		districtInput := strings.ToLower(strings.TrimSpace(*filters.District))
		w.add("NOT (LOWER(loc.district) = ANY($1) OR LOWER(loc.state) = $2)",
			expandDistrictAliases(*filters.District), districtInput)
	}
	return w
}

// referencePoint returns the search center for the fallback radius: the
// average coordinates of the district's own listings if it has any, or —
// for the motivating zero-listing case — the average coordinates of every
// listing in the same state instead. Both are derived entirely from real
// listing data; nothing here is a hardcoded coordinate.
func (s *Service) referencePoint(ctx context.Context, w *whereBuilder, filters *Filters) (lat, lon float64, ok bool, err error) {
	primaryQuery := fmt.Sprintf(`
SELECT avg(l.latitude), avg(l.longitude)
FROM listings l
LEFT JOIN locations loc ON l.location_id = loc.location_id
LEFT JOIN property_types pt ON l.property_type_id = pt.id
LEFT JOIN stay_types st ON l.stay_type_id = st.id
LEFT JOIN LATERAL (
  SELECT avg(r.rating) AS avg_rating FROM review r WHERE r.listing_id = l.listing_id
) rv ON TRUE
WHERE %s AND l.latitude IS NOT NULL AND l.longitude IS NOT NULL
`, w.clause())

	var pLat, pLon *float64
	if err := s.pool.QueryRow(ctx, primaryQuery, w.args...).Scan(&pLat, &pLon); err != nil {
		return 0, 0, false, fmt.Errorf("reference point (district): %w", err)
	}
	if pLat != nil && pLon != nil {
		return *pLat, *pLon, true, nil
	}

	if filters == nil || filters.District == nil || *filters.District == "" {
		return 0, 0, false, nil
	}

	var state string
	err = s.pool.QueryRow(ctx,
		`SELECT state FROM locations WHERE LOWER(district) = ANY($1) LIMIT 1`,
		expandDistrictAliases(*filters.District),
	).Scan(&state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("looking up district's state: %w", err)
	}

	var sLat, sLon *float64
	stateQuery := `
SELECT avg(l.latitude), avg(l.longitude)
FROM listings l
LEFT JOIN locations loc ON l.location_id = loc.location_id
WHERE l.is_active = TRUE AND LOWER(loc.state) = LOWER($1)
  AND l.latitude IS NOT NULL AND l.longitude IS NOT NULL
`
	if err := s.pool.QueryRow(ctx, stateQuery, state).Scan(&sLat, &sLon); err != nil {
		return 0, 0, false, fmt.Errorf("reference point (state fallback): %w", err)
	}
	if sLat != nil && sLon != nil {
		return *sLat, *sLon, true, nil
	}
	return 0, 0, false, nil
}

// expandedBucketCounts returns, for each radius in radiiMeters, the count of
// candidates whose distance falls in (radiiMeters[i-1], radiiMeters[i]] —
// i.e. NOT already counted by a smaller radius — in a single query, so
// planExpansion can find the smallest sufficient radius without one round
// trip per candidate radius.
func (s *Service) expandedBucketCounts(ctx context.Context, wExpanded *whereBuilder, refLat, refLon float64) ([]int64, error) {
	args := append([]interface{}{}, wExpanded.args...)
	args = append(args, refLon, refLat)
	lonIdx := len(args) - 1
	latIdx := len(args)

	distanceExpr := fmt.Sprintf(
		`ST_Distance(
      ST_SetSRID(ST_MakePoint(l.longitude, l.latitude), 4326)::geography,
      ST_SetSRID(ST_MakePoint($%d, $%d), 4326)::geography
    )`, lonIdx, latIdx)

	caseParts := ""
	for i, r := range radiiMeters {
		argIdx := len(args) + 1
		args = append(args, r)
		caseParts += fmt.Sprintf("WHEN dist <= $%d THEN %d\n    ", argIdx, i+1)
	}

	query := fmt.Sprintf(`
SELECT bucket, count(*)
FROM (
  SELECT %s AS dist
  FROM listings l
  LEFT JOIN locations loc ON l.location_id = loc.location_id
  LEFT JOIN property_types pt ON l.property_type_id = pt.id
  LEFT JOIN stay_types st ON l.stay_type_id = st.id
  LEFT JOIN LATERAL (
    SELECT avg(r.rating) AS avg_rating FROM review r WHERE r.listing_id = l.listing_id
  ) rv ON TRUE
  WHERE %s AND l.latitude IS NOT NULL AND l.longitude IS NOT NULL
) sub
CROSS JOIN LATERAL (
  SELECT CASE
    %s
    ELSE NULL
  END AS bucket
) b
WHERE bucket IS NOT NULL
GROUP BY bucket
`, distanceExpr, wExpanded.clause(), caseParts)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("expanded bucket counts: %w", err)
	}
	defer rows.Close()

	counts := make([]int64, len(radiiMeters))
	for rows.Next() {
		var bucket int
		var c int64
		if err := rows.Scan(&bucket, &c); err != nil {
			return nil, fmt.Errorf("scanning bucket count: %w", err)
		}
		counts[bucket-1] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// renumberPlaceholders shifts every $N in clause up by offset, in one pass
// (see whereBuilder.add's comment for why this can't be done with
// sequential string replacement).
func renumberPlaceholders(clause string, offset int) string {
	return placeholderRe.ReplaceAllStringFunc(clause, func(m string) string {
		n := 0
		for _, c := range m[1:] {
			n = n*10 + int(c-'0')
		}
		return fmt.Sprintf("$%d", offset+n)
	})
}

// buildExpandedQuery combines the exact-district matches with the nearby
// fallback matches into one query: the primary group keeps the normal
// default-mode price-tier interleave ordering, the fallback group orders by
// distance, and every fallback row's computed sort key is offset well past
// the range primary rows can ever reach — so, without needing a UNION-level
// GROUP BY or a separate ranking pass, ORDER BY that single key naturally
// yields "all primary results, in their normal order, then all fallback
// results, nearest first."
func buildExpandedQuery(wPrimary, wExpanded *whereBuilder, plan *expansionPlan, pageSize int, cursorOffset int64) (string, []interface{}) {
	args := append([]interface{}{}, wPrimary.args...)
	offset := len(args)
	expandedClause := renumberPlaceholders(wExpanded.clause(), offset)
	args = append(args, wExpanded.args...)

	lonIdx := len(args) + 1
	args = append(args, plan.refLon)
	latIdx := len(args) + 1
	args = append(args, plan.refLat)
	radiusIdx := len(args) + 1
	args = append(args, plan.radiusMeters)

	distanceExpr := fmt.Sprintf(
		`ST_Distance(
      ST_SetSRID(ST_MakePoint(l.longitude, l.latitude), 4326)::geography,
      ST_SetSRID(ST_MakePoint($%d, $%d), 4326)::geography
    )`, lonIdx, latIdx)

	limitIdx := len(args) + 1
	args = append(args, pageSize+1)
	offsetIdx := len(args) + 1
	args = append(args, cursorOffset)

	query := fmt.Sprintf(`
WITH primary_filtered AS (
  SELECT%s
  %s
  WHERE %s
),
primary_tiered AS (
  SELECT *, NTILE(3) OVER (ORDER BY price_weekday NULLS LAST, listing_id) AS _tier
  FROM primary_filtered
),
primary_positioned AS (
  SELECT *, ROW_NUMBER() OVER (PARTITION BY _tier ORDER BY listing_id) - 1 AS _r
  FROM primary_tiered
),
primary_final AS (
  SELECT
    listing_id, title, description, price_weekday, price_weekend,
    num_guests, num_bedrooms, num_beds, num_bathrooms,
    latitude, longitude, property_type_id, stay_type_id, location_id,
    state, district, property_type_name, stay_type_title,
    avg_rating, review_count, listing_media, listing_amenities,
    NULL::float8 AS distance,
    (CASE _tier
      WHEN 2 THEN (_r / 2) * 5 + (CASE WHEN _r %% 2 = 0 THEN 0 ELSE 2 END)
      WHEN 1 THEN (_r / 2) * 5 + (CASE WHEN _r %% 2 = 0 THEN 1 ELSE 3 END)
      WHEN 3 THEN _r * 5 + 4
    END)::float8 AS _sort_key
  FROM primary_positioned
),
expanded_final AS (
  SELECT%s,
    %s AS distance,
    (1000000 + %s) AS _sort_key
  %s
  WHERE %s
    AND l.latitude IS NOT NULL AND l.longitude IS NOT NULL
    AND %s <= $%d
),
combined AS (
  SELECT listing_id, title, description, price_weekday, price_weekend,
    num_guests, num_bedrooms, num_beds, num_bathrooms,
    latitude, longitude, property_type_id, stay_type_id, location_id,
    state, district, property_type_name, stay_type_title,
    avg_rating, review_count, listing_media, listing_amenities,
    distance, _sort_key
  FROM primary_final
  UNION ALL
  SELECT listing_id, title, description, price_weekday, price_weekend,
    num_guests, num_bedrooms, num_beds, num_bathrooms,
    latitude, longitude, property_type_id, stay_type_id, location_id,
    state, district, property_type_name, stay_type_title,
    avg_rating, review_count, listing_media, listing_amenities,
    distance, _sort_key
  FROM expanded_final
)
SELECT
  listing_id, title, description, price_weekday, price_weekend,
  num_guests, num_bedrooms, num_beds, num_bathrooms,
  latitude, longitude, property_type_id, stay_type_id, location_id,
  state, district, property_type_name, stay_type_title,
  avg_rating, review_count, listing_media, listing_amenities,
  distance
FROM combined
ORDER BY _sort_key
LIMIT $%d OFFSET $%d
`, selectCols, joinBlock, wPrimary.clause(),
		selectCols, distanceExpr, distanceExpr, joinBlock, expandedClause, distanceExpr, radiusIdx,
		limitIdx, offsetIdx)

	return query, args
}

// searchWithExpansion runs the combined primary+fallback query and shapes
// the response identically to Search()'s normal path.
func (s *Service) searchWithExpansion(ctx context.Context, wPrimary *whereBuilder, filters *Filters, plan *expansionPlan, pageSize int, cursorOffset int64) (*Response, error) {
	wExpanded := buildExpansionWhereBuilder(filters)
	query, args := buildExpandedQuery(wPrimary, wExpanded, plan, pageSize, cursorOffset)

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
		nextCursor := cursorOffset + int64(len(results))
		resp.Cursor = &nextCursor
	}
	if cursorOffset == 0 {
		total := plan.combinedTotal
		resp.TotalCount = &total
	}
	return resp, nil
}
