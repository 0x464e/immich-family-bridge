package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/httpapi"
	"github.com/0x464e/immich-family-bridge/internal/immich/httpclient"
	"github.com/0x464e/immich-family-bridge/internal/reconcile"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

func main() {
	path := flag.String("config", "/etc/familybridge/config.yaml", "config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c, err := config.Load(*path)
	if err != nil {
		log.Error("configuration invalid", "error", err)
		os.Exit(1)
	}
	db, err := store.Open(c.Database)
	if err != nil {
		log.Error("database unavailable", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	api := httpclient.New(c.ImmichURL)
	api.AdminKey = c.AdminKey
	api.ReadOnly = c.DryRun
	version, err := api.Version(context.Background())
	if err != nil || !strings.HasPrefix(version, "3.") {
		log.Error("unsupported Immich API version", "version", version, "error", err)
		os.Exit(1)
	}
	r := reconcile.New(c, db, api, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := r.Check(ctx); err != nil {
		log.Error("backend identity check failed", "error", err)
		os.Exit(1)
	}
	if err := db.Init(c.FamilyID, c.Members); err != nil {
		log.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	interval, _ := c.Interval()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if c.DryRun {
					preview, e := r.DryRun(ctx)
					if e != nil {
						log.Error("dry-run preview incomplete", "error", e)
					} else {
						log.Info("dry-run preview", "action_count", preview["count"])
					}
					continue
				}
				if e := r.Run(ctx); e != nil {
					log.Error("reconciliation cycle incomplete", "error", e)
				}
			}
		}
	}()
	srv := &http.Server{Addr: c.Listen, Handler: (&httpapi.Server{C: c, DB: db, R: r}).Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	mode := "active"
	if c.DryRun {
		mode = "dry_run"
	}
	log.Info("Immich Family Bridge started", "listen", c.Listen, "immich_version", version, "mode", mode)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
}
