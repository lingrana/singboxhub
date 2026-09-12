package webui

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/config"
	"github.com/sing-hub/panel/internal/store"
)

type setupResult struct {
	selection config.DatabaseSelection
	err       error
}

// RunDatabaseSetup serves the first-run database selection page and returns
// only after a database has been opened, migrated, and its choice persisted.
func RunDatabaseSetup(ctx context.Context, listenAddr, dataDir string, logger *slog.Logger) (config.DatabaseSelection, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return config.DatabaseSelection{}, err
	}
	rd, err := NewRenderer()
	if err != nil {
		_ = ln.Close()
		return config.DatabaseSelection{}, err
	}
	result := make(chan setupResult, 1)
	var once sync.Once
	defaultSQLite := dataDir + "/panel.db"
	mux := http.NewServeMux()
	if static, staticErr := Static(); staticErr == nil {
		mux.Handle("GET /admin/assets/{path...}", cacheStatic(http.StripPrefix("/admin/assets/", http.FileServer(http.FS(static)))))
	}
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = rd.render(w, "setup", setupPageData{SQLiteDSN: defaultSQLite})
	})
	mux.HandleFunc("POST /setup", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			renderSetupError(w, rd, defaultSQLite, "无法读取表单")
			return
		}
		selection := config.DatabaseSelection{
			Driver: strings.ToLower(strings.TrimSpace(r.FormValue("driver"))),
			DSN:    strings.TrimSpace(r.FormValue("dsn")),
		}
		if selection.Driver == "sqlite" && selection.DSN == "" {
			selection.DSN = defaultSQLite
		}
		if err := config.ValidateDatabaseSelection(selection); err != nil {
			renderSetupError(w, rd, defaultSQLite, "数据库类型或连接信息无效")
			return
		}
		probe, err := store.OpenWithConfig(selection.Driver, selection.DSN)
		if err != nil {
			logger.Error("database setup validation failed", "driver", selection.Driver, "error", err)
			renderSetupError(w, rd, defaultSQLite, "数据库连接或初始化失败："+redactDSN(selection.DSN, err.Error()))
			return
		}
		if err := probe.Close(); err != nil {
			renderSetupError(w, rd, defaultSQLite, "数据库已初始化，但关闭连接失败："+redactDSN(selection.DSN, err.Error()))
			return
		}
		if err := config.SaveDatabaseSelection(dataDir, selection); err != nil {
			renderSetupError(w, rd, defaultSQLite, "无法保存数据库选择："+redactDSN(selection.DSN, err.Error()))
			return
		}
		once.Do(func() { result <- setupResult{selection: selection} })
		securityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>初始化完成</title><meta http-equiv=\"refresh\" content=\"2;url=/login\"><p>数据库初始化完成，正在启动面板，即将跳转到登录页…</p>"))
	})
	// First-run catch-all: any other path (/, /login, /admin/*, favicon.ico, …)
	// lands on the setup page instead of a 404, so the first visit to the
	// panel always reaches initialization.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			once.Do(func() { result <- setupResult{err: err} })
		}
	}()
	select {
	case outcome := <-result:
		_ = server.Shutdown(context.Background())
		return outcome.selection, outcome.err
	case <-ctx.Done():
		_ = server.Shutdown(context.Background())
		return config.DatabaseSelection{}, ctx.Err()
	}
}

type setupPageData struct {
	SQLiteDSN string
	Error     string
}

func renderSetupError(w http.ResponseWriter, rd *Renderer, dsn, message string) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = rd.render(w, "setup", setupPageData{SQLiteDSN: dsn, Error: message})
}

// redactDSN keeps connection details (which may carry the password) out of
// the error text shown on the setup page.
func redactDSN(dsn, message string) string {
	if dsn == "" {
		return message
	}
	return strings.ReplaceAll(message, dsn, "«连接信息已隐藏»")
}
