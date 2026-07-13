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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	goredis "github.com/redis/go-redis/v9"

	"github.com/ameNZB/loon/core"
	"github.com/ameNZB/loon/schedule"

	"github.com/ameNZB/loon-baseline/cache"
	cachememory "github.com/ameNZB/loon-baseline/cache/memory"
	cacheredis "github.com/ameNZB/loon-baseline/cache/redis"

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

	// Read-through cache in front of the Newznab responses — the whole point of
	// a read tier. Redis when REDIS_ADDR is set (the deployed shape: many api
	// workers sharing one Redis), in-memory otherwise (dev). Best-effort: a
	// Redis outage degrades to serving straight from the plugin.
	var responses cache.Cache
	if addr := getenv("REDIS_ADDR", ""); addr != "" {
		responses = cacheredis.New(goredis.NewClient(&goredis.Options{Addr: addr}))
		logger.Info("response cache", "backend", "redis", "addr", addr)
	} else {
		responses = cachememory.New()
		logger.Info("response cache", "backend", "memory")
	}

	engine.GET("/healthz", func(g *gin.Context) { g.String(http.StatusOK, "ok") })
	engine.GET("/api", newznab(api, responses)) // Newznab/Torznab: t=caps|search|tvsearch|movie|rss|get
	engine.GET("/rss", newznab(api, responses))
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

// cachedResp is the serialized Newznab response stored in the cache.
type cachedResp struct {
	Body        []byte `json:"b"`
	ContentType string `json:"c"`
	Filename    string `json:"f"`
}

func newznab(api pluginapi.UsenetNewznab, ca cache.Cache) gin.HandlerFunc {
	return func(g *gin.Context) {
		if api == nil {
			g.String(http.StatusServiceUnavailable, "indexer not configured")
			return
		}
		limit, _ := strconv.Atoi(g.Query("limit"))
		offset, _ := strconv.Atoi(g.Query("offset"))
		req := pluginapi.NewznabRequest{
			Function:   g.Query("t"),
			Query:      g.Query("q"),
			Categories: parseCats(g.Query("cat")),
			Limit:      limit,
			Offset:     offset,
			ID:         g.Query("id"),
			BaseURL:    baseURL(g),
			Title:      "loon api",
			APIKey:     g.Query("apikey"),
		}

		// Cache read functions only. t=get streams a (potentially large) NZB —
		// don't hold those in Redis.
		cacheable := ca != nil && req.Function != "get"
		var key string
		if cacheable {
			key = newznabKey(req)
			var cr cachedResp
			if ok, _ := cache.GetJSON(g.Request.Context(), ca, key, &cr); ok {
				writeResp(g, cr, "hit")
				return
			}
		}

		res, err := api.Newznab(g.Request.Context(), req)
		if err != nil {
			g.String(http.StatusInternalServerError, "api error")
			return
		}
		cr := cachedResp{Body: res.Body, ContentType: res.ContentType, Filename: res.Filename}
		if cacheable {
			_ = cache.SetJSON(g.Request.Context(), ca, key, cr, ttlFor(req.Function))
		}
		writeResp(g, cr, "miss")
	}
}

func writeResp(g *gin.Context, cr cachedResp, status string) {
	if cr.Filename != "" {
		g.Header("Content-Disposition", `attachment; filename="`+cr.Filename+`"`)
	}
	g.Header("X-Cache", status)
	g.Data(http.StatusOK, cr.ContentType, cr.Body)
}

// newznabKey hashes the request fields that determine the response. BaseURL is
// excluded (constant per deployment / public host); APIKey is INCLUDED because
// the plugin embeds it in the download links, so two keys must not share an
// entry.
func newznabKey(r pluginapi.NewznabRequest) string {
	payload := struct {
		T, Q  string
		C     []int
		L, O  int
		ID, K string
	}{r.Function, r.Query, r.Categories, r.Limit, r.Offset, r.ID, r.APIKey}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return "newznab:v1:" + hex.EncodeToString(sum[:16])
}

// ttlFor picks a per-function TTL. Caps are ~static (the category tree); search
// / feed results get a short window balancing hit rate against freshness.
func ttlFor(fn string) time.Duration {
	if fn == "caps" {
		return time.Hour
	}
	return 90 * time.Second
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
