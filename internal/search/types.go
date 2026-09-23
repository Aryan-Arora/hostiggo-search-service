package search

// Filters mirrors the `filters` object accepted by the original
// POST /api/search route, plus a new free-text `q` field (requirement: the
// live/complete RPCs had no free-text search; this is the one intentional
// addition to the contract).
type Filters struct {
	Query        *string  `json:"q"`
	StartDate    *string  `json:"startDate"`
	EndDate      *string  `json:"endDate"`
	District     *string  `json:"district"`
	State        *string  `json:"state"`
	MinPrice     *float64 `json:"minPrice"`
	MaxPrice     *float64 `json:"maxPrice"`
	TotalGuests  *int     `json:"totalGuests"`
	Ratings      []int    `json:"ratings"`
	Amenities    []int    `json:"amenities"`
	RoomTypes    []string `json:"roomTypes"`
	PropertyTypes []string `json:"propertyTypes"`
	StayTypes    []string `json:"stayTypes"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
}

// Request is the POST /api/search request body.
type Request struct {
	Filters  *Filters `json:"filters"`
	Cursor   *int64   `json:"cursor"`
	PageSize *int     `json:"pageSize"`
}

type Location struct {
	State    *string `json:"state"`
	District *string `json:"district"`
}

type Media struct {
	MediaURL string `json:"media_url"`
	IsCover  bool   `json:"is_cover"`
}

type AmenityName struct {
	Name string `json:"name"`
}

type ListingAmenity struct {
	Amenities AmenityName `json:"amenities"`
}

// Listing matches the `listing` object shape the frontend already consumes
// from search_listings_by_state, with a few additive fields (property_type_name,
// stay_type_title, avg_rating, review_count) that the live RPC never returned
// but that the "complete spec" (search_listings/listing_search_view) does —
// these are extra keys, so existing frontend code that only reads the
// original fields is unaffected.
type Listing struct {
	ListingID       int64            `json:"listing_id"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	PriceWeekday    *float64         `json:"price_weekday"`
	PriceWeekend    *float64         `json:"price_weekend"`
	NumGuests       *int             `json:"num_guests"`
	NumBedrooms     *int             `json:"num_bedrooms"`
	NumBeds         *int             `json:"num_beds"`
	NumBathrooms    *int             `json:"num_bathrooms"`
	Latitude        *float64         `json:"latitude"`
	Longitude       *float64         `json:"longitude"`
	PropertyTypeID  *int             `json:"property_type_id"`
	StayTypeID      *int             `json:"stay_type_id"`
	LocationID      *int             `json:"location_id"`
	Locations       Location         `json:"locations"`
	ListingMedia    []Media          `json:"listing_media"`
	ListingAmenities []ListingAmenity `json:"listing_amenities"`
	PropertyTypeName *string         `json:"property_type_name,omitempty"`
	StayTypeTitle    *string         `json:"stay_type_title,omitempty"`
	AvgRating        float64         `json:"avg_rating,omitempty"`
	ReviewCount      int             `json:"review_count,omitempty"`
}

type Result struct {
	Listing  Listing  `json:"listing"`
	Distance *float64 `json:"distance"`
}

type StateBounds struct {
	North float64 `json:"north"`
	South float64 `json:"south"`
	East  float64 `json:"east"`
	West  float64 `json:"west"`
}

// Response matches the original POST /api/search response shape exactly.
type Response struct {
	Data        []Result     `json:"data"`
	Cursor      *int64       `json:"cursor"`
	HasMore     bool         `json:"hasMore"`
	TotalCount  *int64       `json:"totalCount"`
	StateBounds *StateBounds `json:"stateBounds,omitempty"`
}
