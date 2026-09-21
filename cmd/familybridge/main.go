package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
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
	togetherID, err := r.EnsureTogether(ctx)
	if err != nil {
		log.Error("Together album initialization failed", "error", err)
		os.Exit(1)
	}
	log.Info("Together album ready for reconciliation", "logical_album_id", togetherID, "album_name", c.TogetherAlbumName)
	interval, _ := c.Interval()
	discoveryEvery, _ := c.DiscoveryEvery()
	lastDiscovery := time.Time{}
	forceDiscovery := true
	runCycle := func() {
		if c.DryRun {
			if !lastDiscovery.IsZero() && time.Since(lastDiscovery) < discoveryEvery {
				return
			}
			actions, err := r.DryRun(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Error("dry-run cycle incomplete; will retry", "error", err)
				}
				return
			}
			logDryRunActions(log, actions)
			lastDiscovery = time.Now()
			return
		}
		full := forceDiscovery || lastDiscovery.IsZero() || time.Since(lastDiscovery) >= discoveryEvery
		var err error
		if full {
			err = r.Run(ctx)
		} else {
			err = r.Work(ctx)
		}
		if full && (err == nil || errors.Is(err, reconcile.ErrPostwork)) {
			lastDiscovery = time.Now()
			forceDiscovery = false
		} else if err != nil && !errors.Is(err, reconcile.ErrPostwork) {
			forceDiscovery = true
		}
		if err != nil && ctx.Err() == nil {
			log.Error("reconciliation cycle incomplete", "error", err)
		}
	}
	pollingDone := make(chan struct{})
	go func() {
		defer close(pollingDone)
		runPolling(ctx, interval, runCycle)
	}()
	srv := &http.Server{Addr: c.Listen, Handler: (&httpapi.Server{C: c, DB: db, R: r, Session: api}).Handler(), ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Warn("HTTP server shutdown incomplete", "error", err)
		}
	}()
	mode := "active"
	if c.DryRun {
		mode = "dry_run"
	}
	log.Info("Immich Family Bridge started", "listen", c.Listen, "immich_version", version, "mode", mode)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("HTTP server failed", "error", err)
	}
	stop()
	<-pollingDone
	<-shutdownDone
	if err := db.Close(); err != nil {
		log.Warn("database close failed", "error", err)
	}
}

func runPolling(ctx context.Context, interval time.Duration, cycle func()) {
	if ctx.Err() != nil {
		return
	}
	cycle()
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
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
