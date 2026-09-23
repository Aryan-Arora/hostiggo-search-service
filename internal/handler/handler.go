// Package handler wires HTTP routes to the search package, matching the
// original Next.js API route contract:
//
//	POST /api/search      -> search.Service.Search
//	GET  /api/locations   -> search.LocationService (q / popular / sample)
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
	search    *search.Service
	locations *search.LocationService
	log       *slog.Logger
	testUI    fs.FS // optional: static test UI served at "/", nil to disable
}

func New(s *search.Service, l *search.LocationService, log *slog.Logger, testUI fs.FS) *Handler {
	return &Handler{search: s, locations: l, log: log, testUI: testUI}
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

// Routes returns the configured mux.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/search", h.PostSearch)
	mux.HandleFunc("GET /api/locations", h.GetLocations)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if h.testUI != nil {
		// Manual test UI, not part of the API contract — same-origin static
		// page so it can call /api/* with plain fetch(), no CORS needed.
		mux.Handle("/", http.FileServerFS(h.testUI))
	}
	return mux
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
