package main

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Aryan-Arora/hostiggo-search-service/internal/config"
	"github.com/Aryan-Arora/hostiggo-search-service/internal/db"
	"github.com/Aryan-Arora/hostiggo-search-service/internal/handler"
	"github.com/Aryan-Arora/hostiggo-search-service/internal/search"
)

// testUIFiles is a small manual test console for exercising POST /api/search
// and GET /api/locations from a browser. Not part of the API contract;
// embedded so it ships in the same container image as the binary. Set
// DISABLE_TEST_UI=1 to turn it off (e.g. in a real deployment).
//
//go:embed testui
var testUIFiles embed.FS

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config error", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.New(ctx, cfg)
	if err != nil {
		log.Error("database connection failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	searchSvc := search.NewService(pool)
	locationsSvc := search.NewLocationService(pool)
	hotelsSvc := search.NewHotelsService(pool)

	var testUI fs.FS
	if os.Getenv("DISABLE_TEST_UI") != "1" {
		testUI, err = fs.Sub(testUIFiles, "testui")
		if err != nil {
			log.Error("test UI embed error", "err", err)
			os.Exit(1)
		}
	}
	h := handler.New(searchSvc, locationsSvc, hotelsSvc, log, testUI, cfg.CORSAllowOrigin)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}
