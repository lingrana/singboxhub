package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sing-hub/panel/internal/api"
	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/config"
	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/monitor"
	"github.com/sing-hub/panel/internal/sampler"
	"github.com/sing-hub/panel/internal/store"
	"github.com/sing-hub/panel/internal/webui"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := prepareDataDirs(cfg); err != nil {
		logger.Error("prepare data directories", "error", err)
		os.Exit(1)
	}

	db, setupCreds, err := openStore(ctx, cfg, logger)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	cryptoKey, err := cryptox.LoadOrCreateKey(cfg.DataDir, "secret.key")
	if err != nil {
		slog.Error("load crypto key", "error", err)
		os.Exit(1)
	}

	jwtKey, err := cryptox.LoadOrCreateKey(cfg.DataDir, "jwt.key")
	if err != nil {
		slog.Error("load jwt key", "error", err)
		os.Exit(1)
	}

	created, err := initializeAdmin(db, cfg.DataDir, setupCreds.Username, setupCreds.Password)
	if err != nil {
		logger.Error("initialize admin", "error", err)
		os.Exit(1)
	}
	if created {
		logger.Info("created default admin user")
	}
	if _, err := db.GetSettings(store.SamplerDefaults{
		SamplerIntervalSeconds: int(cfg.Sampler.ConnInterval.Seconds()),
		RetentionDays:          cfg.Sampler.RetentionDays,
		IPLookupProviderURL:    cfg.Probe.IPProviderURL,
	}); err != nil {
		logger.Error("initialize settings", "error", err)
		os.Exit(1)
	}

	authMgr := auth.NewManager(jwtKey, cfg.Session.AccessTokenTTL, cfg.Session.RefreshTokenTTL, db)
	hub := sampler.NewHub(db, cryptoKey, logger, cfg.Sampler.PersistInterval)
	mon := monitor.NewMonitor(db, cryptoKey, logger)

	hub.Start(ctx)
	mon.Start(ctx)

	srv := api.NewServer(cfg, logger, db, cryptoKey, authMgr, hub)
	handler := srv.Routes()

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("listen", "error", err)
		os.Exit(1)
	}
	logger.Info("starting panel", "listen", cfg.ListenAddr)

	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		httpSrv.Close()
	}()
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
	}
	hub.Stop()
	mon.Stop()
}

func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (*store.Store, webui.SetupCredentials, error) {
	if cfg.DBDriver != "" {
		dsn := cfg.DBDSN
		if cfg.DBDriver == "sqlite" && dsn == "" {
			dsn = cfg.DBPath
		}
		db, err := store.OpenWithConfig(cfg.DBDriver, dsn)
		return db, webui.SetupCredentials{}, err
	}
	if cfg.DBDSN != "" {
		return nil, webui.SetupCredentials{}, fmt.Errorf("db_dsn requires db_driver")
	}
	selection, exists, err := config.LoadDatabaseSelection(cfg.DataDir)
	if err != nil {
		return nil, webui.SetupCredentials{}, err
	}
	defaultPath := filepath.Clean(filepath.Join(cfg.DataDir, "panel.db"))
	legacyPath := filepath.Clean(cfg.DBPath)
	if exists {
		db, err := store.OpenWithConfig(selection.Driver, selection.DSN)
		return db, webui.SetupCredentials{}, err
	}
	if legacyPath != defaultPath {
		db, err := store.Open(cfg.DBPath)
		return db, webui.SetupCredentials{}, err
	}
	if _, err := os.Stat(defaultPath); err == nil {
		db, err := store.Open(defaultPath)
		return db, webui.SetupCredentials{}, err
	} else if !os.IsNotExist(err) {
		return nil, webui.SetupCredentials{}, err
	}
	logger.Info("database is not configured; waiting for first-run setup", "listen", cfg.ListenAddr)
	selection, credentials, err := webui.RunDatabaseSetup(ctx, cfg.ListenAddr, cfg.DataDir, logger)
	if err != nil {
		return nil, webui.SetupCredentials{}, err
	}
	db, err := store.OpenWithConfig(selection.Driver, selection.DSN)
	return db, credentials, err
}

func extractPassword(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "Initial") {
			return line
		}
	}
	return strings.TrimSpace(raw)
}
