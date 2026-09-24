package search

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultHotelsLimit = 4
	maxHotelsLimit     = 100
)

// HotelsService backs GET /api/hotels — a location-scoped listing sample
// used by the homepage teaser, not general search. Kept separate from
// Service (search.go) since it's a much simpler single-equality query with
// no filters, cursor, or ranking.
type HotelsService struct {
	pool *pgxpool.Pool
}

func NewHotelsService(pool *pgxpool.Pool) *HotelsService {
	return &HotelsService{pool: pool}
}

func clampHotelsLimit(n int) int {
	if n < 1 {
		return defaultHotelsLimit
	}
	if n > maxHotelsLimit {
		return maxHotelsLimit
	}
	return n
}

// Sample returns up to `limit` active listings for a given location_id,
// shaped like the `listing` object in POST /api/search's results (same
// joins, same fields) so a caller already rendering search-result cards can
// reuse that rendering logic for the homepage teaser.
func (s *HotelsService) Sample(ctx context.Context, locationID int64, limit int) ([]Listing, error) {
	limit = clampHotelsLimit(limit)

	rows, err := s.pool.Query(ctx, `
SELECT
  l.listing_id, l.title, l.description, l.price_weekday, l.price_weekend,
  l.num_guests, l.num_bedrooms, l.num_beds, l.num_bathrooms,
  l.latitude, l.longitude, l.property_type_id, l.stay_type_id, l.location_id,
  loc.state, loc.district,
  pt.name AS property_type_name, st.title AS stay_type_title,
  COALESCE(rv.avg_rating, 0) AS avg_rating,
  COALESCE(rv.review_count, 0) AS review_count,
  COALESCE(med.media, '[]'::jsonb) AS listing_media,
  COALESCE(am.amenities, '[]'::jsonb) AS listing_amenities
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
WHERE l.is_active = TRUE AND l.location_id = $1
ORDER BY l.listing_id
LIMIT $2
`, locationID, limit)
	if err != nil {
		return nil, fmt.Errorf("hotels query: %w", err)
	}
	defer rows.Close()

	out := make([]Listing, 0, limit)
	for rows.Next() {
		var (
			l             Listing
			mediaJSON     []byte
			amenitiesJSON []byte
		)
		if err := rows.Scan(
			&l.ListingID, &l.Title, &l.Description, &l.PriceWeekday, &l.PriceWeekend,
			&l.NumGuests, &l.NumBedrooms, &l.NumBeds, &l.NumBathrooms,
			&l.Latitude, &l.Longitude, &l.PropertyTypeID, &l.StayTypeID, &l.LocationID,
			&l.Locations.State, &l.Locations.District,
			&l.PropertyTypeName, &l.StayTypeTitle,
			&l.AvgRating, &l.ReviewCount,
			&mediaJSON, &amenitiesJSON,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		if err := json.Unmarshal(mediaJSON, &l.ListingMedia); err != nil {
			return nil, fmt.Errorf("decoding listing_media: %w", err)
		}
		if err := json.Unmarshal(amenitiesJSON, &l.ListingAmenities); err != nil {
			return nil, fmt.Errorf("decoding listing_amenities: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}
	return out, nil
}
