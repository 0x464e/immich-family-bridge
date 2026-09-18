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
	if c.DryRun {
		if albums, err := db.Albums(); err == nil && len(albums) == 0 {
			log.Info("dry-run has no registered albums; register one through the internal API to preview sharing")
		}
	}
	interval, _ := c.Interval()
	runCycle := func() {
		if c.DryRun {
			actions, err := r.DryRun(ctx)
			if err != nil {
				log.Error("dry-run cycle incomplete; will retry", "error", err)
				return
			}
			logDryRunActions(log, actions)
			return
		}
		if err := r.Run(ctx); err != nil {
			log.Error("reconciliation cycle incomplete", "error", err)
		}
	}
	go runPolling(ctx, interval, runCycle)
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

func runPolling(ctx context.Context, interval time.Duration, cycle func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	cycle()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle()
		}
	}
}

func logDryRunActions(log *slog.Logger, actions []reconcile.Action) {
	for _, action := range actions {
		attrs := []any{"kind", action.Kind, "album_id", action.AlbumID, "album_name", action.AlbumName}
		if action.MemberID != "" {
			attrs = append(attrs, "member_id", action.MemberID)
		}
		if action.SourceMemberID != "" {
			attrs = append(attrs, "source_member_id", action.SourceMemberID)
		}
		if action.LogicalAssetID != "" {
			attrs = append(attrs, "logical_asset_id", action.LogicalAssetID)
		}
		if action.ImmichAssetID != "" {
			attrs = append(attrs, "immich_asset_id", action.ImmichAssetID)
		}
		if action.Error != "" {
			attrs = append(attrs, "error", action.Error)
		}
		switch action.Kind {
		case "mapping_inconsistency", "unsupported_asset", "source_error", "source_missing":
			log.Warn(action.Message(), attrs...)
		default:
			log.Info(action.Message(), attrs...)
		}
	}
	log.Info("dry-run cycle complete", "action_count", len(actions))
}
