package main

import (
	"context"
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
	h := handler.New(searchSvc, locationsSvc, log)

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
