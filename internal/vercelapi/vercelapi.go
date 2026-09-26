// Package vercelapi adapts the existing net/http handler for Vercel's Go
// serverless runtime (each file under /api becomes its own function, each
// exporting a Handler(w, r) func — see api/search/index.go etc.). The pool
// and handler are created once per warm function instance via sync.Once,
// not per request.
package vercelapi

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/Aryan-Arora/search-service-backend/internal/config"
	"github.com/Aryan-Arora/search-service-backend/internal/db"
	apihandler "github.com/Aryan-Arora/search-service-backend/internal/handler"
	"github.com/Aryan-Arora/search-service-backend/internal/search"
)

var (
	once    sync.Once
	h       *apihandler.Handler
	initErr error
)

func ensureInit() error {
	once.Do(func() {
		cfg, err := config.Load()
		if err != nil {
			initErr = err
			return
		}
		pool, err := db.New(context.Background(), cfg)
		if err != nil {
			initErr = err
			return
		}
		searchSvc := search.NewService(pool)
		locationsSvc := search.NewLocationService(pool)
		hotelsSvc := search.NewHotelsService(pool)
		log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		// No embedded test UI on Vercel — that's a local-dev convenience only.
		h = apihandler.New(searchSvc, locationsSvc, hotelsSvc, log, nil, cfg.CORSAllowOrigin)
	})
	return initErr
}

// Handler is shared verbatim by every /api/*/index.go entrypoint. Routing
// still works correctly per-function because Vercel forwards the original
// request path unchanged, and the underlying mux matches on that path.
func Handler(w http.ResponseWriter, r *http.Request) {
	if err := ensureInit(); err != nil {
		http.Error(w, "service init failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.Routes().ServeHTTP(w, r)
}
