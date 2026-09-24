// Package handler wires HTTP routes to the search package, matching the
// original Next.js API route contract:
//
//	POST /api/search      -> search.Service.Search
//	GET  /api/locations   -> search.LocationService (q / popular / sample)
//	GET  /api/hotels      -> search.HotelsService (location-scoped sample)
package handler

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Aryan-Arora/hostiggo-search-service/internal/search"
)

type Handler struct {
	search     *search.Service
	locations  *search.LocationService
	hotels     *search.HotelsService
	log        *slog.Logger
	testUI     fs.FS  // optional: static test UI served at "/", nil to disable
	corsOrigin string // "" disables CORS headers entirely
}

func New(s *search.Service, l *search.LocationService, h *search.HotelsService, log *slog.Logger, testUI fs.FS, corsOrigin string) *Handler {
	return &Handler{search: s, locations: l, hotels: h, log: log, testUI: testUI, corsOrigin: corsOrigin}
}

func jsonError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Routes returns the configured mux, wrapped with CORS handling.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/search", h.PostSearch)
	mux.HandleFunc("GET /api/locations", h.GetLocations)
	mux.HandleFunc("GET /api/hotels", h.GetHotels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if h.testUI != nil {
		// Manual test UI, not part of the API contract — same-origin static
		// page so it can call /api/* with plain fetch(), no CORS needed.
		mux.Handle("/", http.FileServerFS(h.testUI))
	}
	return h.withCORS(mux)
}

// withCORS adds permissive CORS headers so a frontend on a different origin
// can call this API directly with fetch(). This API is fully public/
// anonymous already (no auth header, no cookies — same as the original
// Supabase anon-key search path), so this doesn't widen access, it just lets
// a browser reach it cross-origin. Handles the preflight OPTIONS request
// browsers send before a cross-origin POST/fetch with a JSON body.
func (h *Handler) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.corsOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", h.corsOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) PostSearch(w http.ResponseWriter, r *http.Request) {
	var req search.Request
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			// The original route did not validate malformed bodies before
			// passing them to Postgres and returned a generic 500; we match
			// that behavior rather than introducing new 4xx semantics.
			jsonError(w, http.StatusInternalServerError, err)
			return
		}
	}

	resp, err := h.search.Search(r.Context(), req)
	if err != nil {
		h.log.Error("search failed", "err", err)
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) GetLocations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			limit = n
		}
	}

	var (
		rows []search.LocationRow
		err  error
	)

	switch {
	case q.Get("q") != "":
		rows, err = h.locations.Search(r.Context(), q.Get("q"), limit)
		w.Header().Set("Cache-Control", "no-store")
	case q.Get("popular") == "1":
		rows, err = h.locations.Popular(r.Context(), limit)
		w.Header().Set("Cache-Control", "public, s-maxage=60, stale-while-revalidate=300")
	default:
		rows, err = h.locations.Sample(r.Context(), limit)
		w.Header().Set("Cache-Control", "public, s-maxage=60, stale-while-revalidate=300")
	}

	if err != nil {
		h.log.Error("locations failed", "err", err)
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": rows})
}

// GetHotels backs the homepage teaser. Deliberately lenient on bad/missing
// input — a missing or non-numeric locationId returns an empty result
// rather than a 4xx/5xx, since this endpoint backs a homepage widget that
// should never break the page it's embedded in.
func (h *Handler) GetHotels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}

	w.Header().Set("Cache-Control", "public, s-maxage=30, stale-while-revalidate=120")

	locationID, err := strconv.ParseInt(q.Get("locationId"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"data": []search.Listing{}})
		return
	}

	rows, err := h.hotels.Sample(r.Context(), locationID, limit)
	if err != nil {
		// Same reasoning as above: log it, but don't break the homepage over
		// a teaser widget failing.
		h.log.Error("hotels failed", "err", err)
		writeJSON(w, http.StatusOK, map[string]interface{}{"data": []search.Listing{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": rows})
}
