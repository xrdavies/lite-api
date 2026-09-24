// Package app implements the single-instance HTTP service.
package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/xrdavies/lite-api/schema"
)

type Config struct {
	DatabaseURL, RedisURL, ListenAddr, JWTSecret, UpstreamPrivateCIDRs, PricingFile string
	BalanceCheckEnabled, BalanceThreshold, BalanceCheckIntervalMinutes              string
	StreamDataIntervalTimeout, ImageStreamDataIntervalTimeout                       string
	GeminiQuotaPolicy                                                               string
}

func ConfigFromEnv() Config {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	return Config{
		DatabaseURL: os.Getenv("DATABASE_URL"), RedisURL: os.Getenv("REDIS_URL"), ListenAddr: addr,
		JWTSecret: os.Getenv("JWT_SECRET"), UpstreamPrivateCIDRs: os.Getenv("UPSTREAM_PRIVATE_CIDRS"), PricingFile: os.Getenv("PRICING_FILE"),
		BalanceCheckEnabled:            os.Getenv("GATEWAY_CN_PROVIDERS_BALANCE_CHECK_ENABLED"),
		BalanceThreshold:               os.Getenv("GATEWAY_CN_PROVIDERS_BALANCE_THRESHOLD"),
		BalanceCheckIntervalMinutes:    os.Getenv("GATEWAY_CN_PROVIDERS_BALANCE_CHECK_INTERVAL_MINUTES"),
		StreamDataIntervalTimeout:      os.Getenv("GATEWAY_STREAM_DATA_INTERVAL_TIMEOUT"),
		ImageStreamDataIntervalTimeout: os.Getenv("GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT"),
		GeminiQuotaPolicy:              os.Getenv("GEMINI_QUOTA_POLICY"),
	}
}

type App struct {
	DB                *sql.DB
	Redis             *redis.Client
	instanceLock      *sql.Conn
	secret            []byte
	mux               *http.ServeMux
	privateUpstreams  []netip.Prefix
	workerCancel      context.CancelFunc
	workerDone        chan struct{}
	imageWorkerDone   chan struct{}
	imageTaskMu       sync.Mutex
	batchMu           sync.Mutex
	batchWorkerDone   chan struct{}
	videoWorkerDone   chan struct{}
	balanceWorkerDone chan struct{}
	billingWorkerDone chan struct{}
	proxyWorkerDone   chan struct{}
	balancePolicy     balanceCheckPolicy
	balanceMu         sync.Mutex
	planMu            sync.Mutex
	instanceLost      atomic.Bool
	ingressFailures   atomic.Uint64
	ingressDropped    atomic.Uint64
	gatewayMu         sync.Mutex
	gatewayActive     map[string]int
	gatewayWaiting    map[string]int
	gatewayQueued     int
	gatewayWake       chan struct{}
	gatewayStopped    bool
	websockets        map[*responseSocket]context.CancelFunc
	websocketDone     sync.WaitGroup
	priceFile         string
	priceMu           sync.Mutex
	prices            atomic.Pointer[priceCatalog]
	panelMu           sync.Mutex
	panelCache        *panelRateSettings
	panelExpires      time.Time
	streamIdle        time.Duration
	imageStreamIdle   time.Duration
	geminiQuotaPolicy geminiQuotaPolicy

	responseWorkerDone chan struct{}
}

func OpenDatabase(ctx context.Context, url string) (*sql.DB, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("database connection failed")
	}
	return db, nil
}

func New(ctx context.Context, cfg Config) (*App, error) {
	var quotaPolicy geminiQuotaPolicy
	if strings.TrimSpace(cfg.GeminiQuotaPolicy) != "" {
		var err error
		quotaPolicy, err = parseGeminiQuotaPolicy([]byte(cfg.GeminiQuotaPolicy), true)
		if err != nil {
			return nil, errors.New("invalid GEMINI_QUOTA_POLICY")
		}
	}
	streamIdle, imageStreamIdle, err := parseStreamIntervals(cfg)
	if err != nil {
		return nil, err
	}
	balancePolicy, err := parseBalancePolicy(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.JWTSecret) < 32 {
		return nil, errors.New("JWT_SECRET must contain at least 32 bytes")
	}
	pricing := &App{priceFile: cfg.PricingFile}
	if err := pricing.reloadPrices(); err != nil {
		return nil, err
	}
	db, err := OpenDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*App, error) { db.Close(); return nil, err }
	if err = schema.Validate(ctx, db); err != nil {
		return fail(err)
	}
	instanceLock, err := db.Conn(ctx)
	if err != nil {
		return fail(err)
	}
	var locked bool
	if err = instanceLock.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(720033)").Scan(&locked); err != nil || !locked {
		instanceLock.Close()
		if err != nil {
			return fail(err)
		}
		return fail(errors.New("another lite-api instance is already serving this database"))
	}
	fail = func(err error) (*App, error) { instanceLock.Close(); db.Close(); return nil, err }
	if cfg.RedisURL == "" {
		return fail(errors.New("REDIS_URL is required"))
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fail(errors.New("invalid Redis configuration"))
	}
	cache := redis.NewClient(opts)
	if err = cache.Ping(ctx).Err(); err != nil {
		cache.Close()
		return fail(errors.New("Redis connection failed"))
	}
	a := &App{DB: db, Redis: cache, instanceLock: instanceLock, secret: []byte(cfg.JWTSecret), mux: http.NewServeMux()}
	a.streamIdle, a.imageStreamIdle = streamIdle, imageStreamIdle
	a.geminiQuotaPolicy = quotaPolicy
	a.balancePolicy = balancePolicy
	a.priceFile = cfg.PricingFile
	a.prices.Store(pricing.prices.Load())
	for _, raw := range strings.Split(cfg.UpstreamPrivateCIDRs, ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			cache.Close()
			return fail(errors.New("invalid UPSTREAM_PRIVATE_CIDRS"))
		}
		a.privateUpstreams = append(a.privateUpstreams, prefix)
	}
	a.routes()
	a.startWorkers()
	return a, nil
}
func (a *App) Close() {
	a.StopAdmission()
	a.websocketDone.Wait()
	a.workerCancel()
	<-a.workerDone
	<-a.imageWorkerDone
	<-a.batchWorkerDone
	<-a.videoWorkerDone
	<-a.responseWorkerDone
	<-a.balanceWorkerDone
	<-a.billingWorkerDone
	<-a.proxyWorkerDone
	a.Redis.Close()
	a.instanceLock.Close()
	a.DB.Close()
}
func (a *App) Handler() http.Handler { return a.mux }

func (a *App) checkInstance(ctx context.Context) error {
	if a.instanceLost.Load() {
		return &apiError{503, "instance lock lost; restart this service"}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := a.instanceLock.PingContext(ctx); err != nil {
		a.instanceLost.Store(true)
		a.StopAdmission()
		return &apiError{503, "instance lock lost; restart this service"}
	}
	return nil
}

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string   { return e.message }
func bad(message string) error      { return &apiError{400, message} }
func unauthorized() error           { return &apiError{401, "invalid credentials or expired session"} }
func denied() error                 { return &apiError{403, "permission denied"} }
func missing() error                { return &apiError{404, "resource not found"} }
func conflict(message string) error { return &apiError{409, message} }

type handler func(http.ResponseWriter, *http.Request) error

func (a *App) route(pattern, access string, h handler) {
	a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		requestID := randomToken(18)
		w.Header().Set("X-Request-ID", requestID)
		status := 200
		started := time.Now()
		var user *identity
		err := func() error {
			if err := a.checkInstance(r.Context()); err != nil {
				return err
			}
			if access != "public" {
				var err error
				if access == "admin" {
					user, err = a.authenticateAdmin(r)
				} else {
					user, err = a.authenticate(r)
				}
				if err != nil {
					return err
				}
				if access == "admin" && user.Role != "admin" {
					return denied()
				}
				r = r.WithContext(context.WithValue(r.Context(), identityKey{}, user))
			}
			if err := a.panelRateLimit(w, r, user); err != nil {
				return err
			}
			return h(w, r)
		}()
		if err != nil {
			status = a.writeError(w, err)
		}
		if user != nil && r.Method != "GET" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = a.DB.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,actor_email,actor_role,auth_method,action,method,path,request_id,client_ip,status_code,latency_ms) VALUES($1,$2,$3,$11,$4,$5,$6,$7,$8,$9,$10)`, user.ID, user.Email, user.Role, pattern, r.Method, r.URL.Path, requestID, clientIP(r), status, time.Since(started).Milliseconds(), user.AuthMethod)
			if err != nil {
				slog.Error("audit record failed", "request_id", requestID)
			}
		}
	})
}
func (a *App) writeError(w http.ResponseWriter, err error) int {
	status, message := 500, "internal error"
	var e *apiError
	var pe *pq.Error
	switch {
	case errors.As(err, &e):
		status, message = e.status, e.message
	case errors.Is(err, sql.ErrNoRows):
		status, message = 404, "resource not found"
	case errors.As(err, &pe) && pe.Code == "23505":
		status, message = 409, "resource already exists"
	case errors.As(err, &pe) && pe.Code == "23503":
		status, message = 400, "invalid referenced resource"
	default:
		slog.Error("request failed", "error_type", fmt.Sprintf("%T", err))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"code": status, "message": message})
	return status
}
func reply(w http.ResponseWriter, data any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": data})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return bad("invalid JSON body or unsupported field")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return bad("exactly one JSON object is required")
	}
	return nil
}
func randomToken(n int) string { return base64.RawURLEncoding.EncodeToString(randomBytes(n)) }
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, bad("invalid id")
	}
	return id, nil
}
func pagination(r *http.Request) (int, int) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	n, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if n == 0 {
		n, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	}
	if p < 1 || p > 1000000 {
		p = 1
	}
	if n < 1 || n > 1000 {
		n = 20
	}
	return p, n
}
func pageReply(w http.ResponseWriter, items any, total, page, size int) error {
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	return reply(w, map[string]any{"items": items, "total": total, "page": page, "page_size": size, "pages": pages})
}
func jsonRow(row *sql.Row) (json.RawMessage, error) {
	var raw []byte
	err := row.Scan(&raw)
	return json.RawMessage(raw), err
}
func jsonRows(rows *sql.Rows) ([]json.RawMessage, error) {
	defer rows.Close()
	result := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		result = append(result, json.RawMessage(b))
	}
	return result, rows.Err()
}
func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return token
}

func (a *App) routes() {
	// Client telemetry is acknowledged and discarded without reading or storing it.
	a.mux.HandleFunc("POST /api/event_logging/batch", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	a.route("GET /health", "public", func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if a.DB.PingContext(ctx) != nil || a.Redis.Ping(ctx).Err() != nil {
			return &apiError{503, "service dependencies unavailable"}
		}
		return reply(w, map[string]string{"status": "ok"})
	})
	a.settingsRoutes()
	a.webSearchRoutes()
	a.runtimeSettingsRoutes()
	a.errorRuleRoutes()
	a.tlsProfileRoutes()
	a.adminKeyRoutes()
	a.imageStorageRoutes()
	a.imageTaskRoutes()
	a.batchImageRoutes()
	a.videoRoutes()
	version := func(w http.ResponseWriter, r *http.Request) error {
		return reply(w, map[string]string{"version": "dev"})
	}
	a.route("GET /api/v1/version", "public", version)
	a.route("GET /api/v1/admin/system/version", "admin", version)
	a.authRoutes()
	a.userRoutes()
	a.keyRoutes()
	a.groupRoutes()
	a.channelRoutes()
	a.accountRoutes()
	a.proxyRoutes()
	a.testPlanRoutes()
	a.gatewayRoutes()
	a.modelRoutes()
	a.quotaRoutes()
	a.usageRoutes()
	a.operationalRoutes()
	a.route("GET /api/v1/admin/ops/concurrency", "admin", a.accountConcurrency)
	a.route("GET /api/v1/admin/ops/user-concurrency", "admin", a.userConcurrency)
}
