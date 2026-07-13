// loon-api is a standalone, read-only API host for a loon indexer. It boots loon
// in the "api" process and mounts ONLY the Newznab/Torznab search API + NZB
// download, sharing the Postgres the web/worker processes use. No sessions,
// templates, admin, or view system — a thin, horizontally-scalable read tier
// (run several behind a load balancer; point them at a read replica later).
//
// This is the "separate project" shape of the api worker: the host wiring is
// tiny now that every feature lives in loon + the plugins. The web demo boots
// the same plugins in Process "all"; this boots them in "api", so usenet only
// publishes its read capabilities (no crawl jobs, no admin views).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/ameNZB/loon/core"
	"github.com/ameNZB/loon/schedule"

	"github.com/ameNZB/loon-plugins/pluginapi"
	_ "github.com/ameNZB/loon-plugins/usenet"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	db, err := connect(getenv("LOON_API_DSN", "postgres://demo:demo@localhost:5544/loon_demo?sslmode=disable"))
	if err != nil {
		logger.Error("db connect", "err", err)
		os.Exit(1)
	}

	engine := gin.New()
	engine.Use(gin.Recovery())

	// core.New requires every dep non-nil, but the api process only exercises
	// Storage + Config (usenet). The rest are minimal stubs — no auth, points,
	// notifications, or scheduler work happens here.
	c, err := core.New(core.Deps{
		Process:       "api",
		Users:         core.NewUsers(core.UsersAdapter{}),
		Auth:          core.NewAuth(core.AuthAdapter{}),
		RBAC:          core.NewRBAC(),
		Storage:       core.NewStorage(db),
		Scheduler:     schedule.CoreScheduler(schedule.Default),
		Router:        core.NewRouter(core.RouterAdapter{Engine: engine}),
		Logger:        logger,
		Config:        core.NewConfig(map[string]any{}),
		Notifications: core.NewNotifications(core.NotificationsAdapter{}),
		Points:        core.NewPoints(core.PointsAdapter{}),
		HTTPClient:    core.NewHTTPClient(),
		Errors:        core.NewErrorReporter(core.ErrorAdapter{}),
	})
	if err != nil {
		logger.Error("core.New", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := core.Boot(ctx, c)
	if err != nil {
		logger.Error("core.Boot", "err", err)
		os.Exit(1)
	}
	logger.Info("api process booted", "plugins", len(rt.Plugins()))

	var idx pluginapi.UsenetIndex
	var api pluginapi.UsenetNewznab
	if v, ok := c.Lookup(pluginapi.UsenetIndexName); ok {
		idx, _ = v.(pluginapi.UsenetIndex)
	}
	if v, ok := c.Lookup(pluginapi.UsenetNewznabName); ok {
		api, _ = v.(pluginapi.UsenetNewznab)
	}

	engine.GET("/healthz", func(g *gin.Context) { g.String(http.StatusOK, "ok") })
	engine.GET("/api", newznab(api)) // Newznab/Torznab: t=caps|search|tvsearch|movie|rss|get
	engine.GET("/rss", newznab(api))
	engine.GET("/nzb/:id", nzb(idx))

	srv := &http.Server{Addr: getenv("LOON_API_ADDR", ":8091"), Handler: engine}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", "err", err)
			stop()
		}
	}()
	logger.Info("listening", "addr", srv.Addr)

	<-ctx.Done()
	sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sc)
}

func newznab(api pluginapi.UsenetNewznab) gin.HandlerFunc {
	return func(g *gin.Context) {
		if api == nil {
			g.String(http.StatusServiceUnavailable, "indexer not configured")
			return
		}
		limit, _ := strconv.Atoi(g.Query("limit"))
		offset, _ := strconv.Atoi(g.Query("offset"))
		res, err := api.Newznab(g.Request.Context(), pluginapi.NewznabRequest{
			Function:   g.Query("t"),
			Query:      g.Query("q"),
			Categories: parseCats(g.Query("cat")),
			Limit:      limit,
			Offset:     offset,
			ID:         g.Query("id"),
			BaseURL:    baseURL(g),
			Title:      "loon api",
			APIKey:     g.Query("apikey"),
		})
		if err != nil {
			g.String(http.StatusInternalServerError, "api error")
			return
		}
		if res.Filename != "" {
			g.Header("Content-Disposition", `attachment; filename="`+res.Filename+`"`)
		}
		g.Data(http.StatusOK, res.ContentType, res.Body)
	}
}

func nzb(idx pluginapi.UsenetIndex) gin.HandlerFunc {
	return func(g *gin.Context) {
		if idx == nil {
			g.String(http.StatusServiceUnavailable, "indexer not configured")
			return
		}
		id, _ := strconv.ParseInt(g.Param("id"), 10, 64)
		data, filename, err := idx.NZB(g.Request.Context(), id)
		if err != nil {
			g.String(http.StatusNotFound, "not found")
			return
		}
		g.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		g.Data(http.StatusOK, "application/x-nzb", data)
	}
}

func parseCats(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func baseURL(g *gin.Context) string {
	scheme := "http"
	if g.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + g.Request.Host
}

func connect(dsn string) (*sqlx.DB, error) {
	var db *sqlx.DB
	var err error
	for i := 0; i < 10; i++ {
		if db, err = sqlx.Connect("postgres", dsn); err == nil {
			return db, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, err
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
