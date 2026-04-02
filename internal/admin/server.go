package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"2scloud-edge-gateway/internal/config"
	"2scloud-edge-gateway/internal/utils"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"golang.org/x/crypto/bcrypt"
)

// ─── Module registry ──────────────────────────────────────────────────────────

type ModuleInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status"` // "healthy" | "error" | "disabled" | "loading"
	Route      string `json:"route"`
	TimeoutMs  int    `json:"timeout_ms"`
	ConfigMode string `json:"config_mode"`
	Version    string `json:"version,omitempty"`
}

type ModuleRegistry struct {
	mu      sync.RWMutex
	modules map[string]*ModuleInfo
}

func newModuleRegistry(cfgs []config.ModuleCfg) *ModuleRegistry {
	reg := &ModuleRegistry{modules: make(map[string]*ModuleInfo)}
	for _, m := range cfgs {
		status := "healthy"
		if !m.Enabled {
			status = "disabled"
		}
		name := strings.ToUpper(m.ID[:1]) + m.ID[1:]
		reg.modules[m.ID] = &ModuleInfo{
			ID:         m.ID,
			Name:       name,
			Enabled:    m.Enabled,
			Status:     status,
			Route:      m.Route,
			TimeoutMs:  m.TimeoutMs,
			ConfigMode: m.ConfigMode,
			Version:    "1.0.0",
		}
	}
	return reg
}

func (r *ModuleRegistry) list() []ModuleInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModuleInfo, 0, len(r.modules))
	for _, m := range r.modules {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *ModuleRegistry) patch(id string, enabled *bool) (*ModuleInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.modules[id]
	if !ok {
		return nil, false
	}
	if enabled != nil {
		m.Enabled = *enabled
		if *enabled {
			m.Status = "healthy"
		} else {
			m.Status = "disabled"
		}
	}
	cp := *m
	return &cp, true
}

// ─── WAF rules ────────────────────────────────────────────────────────────────

type WafRuleFile struct {
	Version int        `json:"version"`
	Rules   []WafRule  `json:"rules"`
}

type WafRule struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Type        string   `json:"type"`
	Values      []string `json:"values"`
	Action      string   `json:"action"`
}

// ─── Rate-limit config ────────────────────────────────────────────────────────

type RateLimitCfg struct {
	MaxRequests   int `json:"max_requests"`
	WindowSeconds int `json:"window_seconds"`
}

// ─── JWT helpers ──────────────────────────────────────────────────────────────

type claims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func generateToken(username, secret string, ttlHours int) (string, error) {
	exp := time.Now().Add(time.Duration(ttlHours) * time.Hour)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	return tok.SignedString([]byte(secret))
}

func parseToken(tokenStr, secret string) (*claims, error) {
	tok, err := jwt.ParseWithClaims(tokenStr, &claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	c, ok := tok.Claims.(*claims)
	if !ok || !tok.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return c, nil
}

func jwtMiddleware(secret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		auth := c.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing token"})
		}
		if _, err := parseToken(auth[7:], secret); err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}
		return c.Next()
	}
}

// ─── Password check ───────────────────────────────────────────────────────────

func checkPassword(hash, plain string) bool {
	// bcrypt hash
	if strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$") {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
	}
	// plaintext fallback (dev only)
	return hash == plain
}

// ─── Metrics response types ───────────────────────────────────────────────────

type RequestMetric struct {
	Timestamp      string  `json:"timestamp"`
	RequestsPerSec float64 `json:"requestsPerSec"`
	LatencyP50     float64 `json:"latencyP50"`
	LatencyP95     float64 `json:"latencyP95"`
	LatencyP99     float64 `json:"latencyP99"`
}

type ErrorMetric struct {
	Timestamp string  `json:"timestamp"`
	ErrorRate float64 `json:"errorRate"`
	Status2xx int64   `json:"status2xx"`
	Status3xx int64   `json:"status3xx"`
	Status4xx int64   `json:"status4xx"`
	Status5xx int64   `json:"status5xx"`
}

type RouteMetric struct {
	Route        string  `json:"route"`
	Method       string  `json:"method"`
	RequestCount int64   `json:"requestCount"`
	AvgLatency   float64 `json:"avgLatency"`
	ErrorRate    float64 `json:"errorRate"`
	P99Latency   float64 `json:"p99Latency"`
}

type StatusDist struct {
	Code       string  `json:"code"`
	Count      int64   `json:"count"`
	Percentage float64 `json:"percentage"`
}

type GatewayStats struct {
	TotalRequests      int64   `json:"totalRequests"`
	TotalRequestsDelta float64 `json:"totalRequestsDelta"`
	AvgLatency         float64 `json:"avgLatency"`
	AvgLatencyDelta    float64 `json:"avgLatencyDelta"`
	ErrorRate          float64 `json:"errorRate"`
	ErrorRateDelta     float64 `json:"errorRateDelta"`
	ActiveRoutes       int     `json:"activeRoutes"`
	Uptime             int64   `json:"uptime"`
}

type DashboardData struct {
	Stats              GatewayStats    `json:"stats"`
	RequestMetrics     []RequestMetric `json:"requestMetrics"`
	ErrorMetrics       []ErrorMetric   `json:"errorMetrics"`
	TopRoutes          []RouteMetric   `json:"topRoutes"`
	StatusDistribution []StatusDist    `json:"statusDistribution"`
}

// ─── Metrics computation ──────────────────────────────────────────────────────

type rangeConfig struct {
	duration  time.Duration
	groupBy   time.Duration
}

var rangeConfigs = map[string]rangeConfig{
	"1h":  {time.Hour, time.Minute},
	"6h":  {6 * time.Hour, 5 * time.Minute},
	"24h": {24 * time.Hour, 15 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 2 * time.Hour},
	"30d": {30 * 24 * time.Hour, 8 * time.Hour},
}

func buildDashboard(s *Store, modules *ModuleRegistry, rangeStr string) DashboardData {
	rc, ok := rangeConfigs[rangeStr]
	if !ok {
		rc = rangeConfigs["24h"]
	}

	since := time.Now().Add(-rc.duration)
	buckets := s.GetBucketsSince(since)

	// ── Group buckets into intervals ──────────────────────────────────────────
	type group struct {
		ts           time.Time
		reqTotal     int64
		blockTotal   int64
		latencySum   int64
		latencyCount int64
		latencies    []int64
		s2xx, s3xx, s4xx, s5xx int64
	}

	grouped := make(map[time.Time]*group)
	for _, b := range buckets {
		ts := b.Timestamp.Truncate(rc.groupBy)
		g, exists := grouped[ts]
		if !exists {
			g = &group{ts: ts}
			grouped[ts] = g
		}
		g.reqTotal += b.RequestCount
		g.blockTotal += b.BlockCount
		g.latencySum += b.LatencySum
		g.latencyCount += b.LatencyCount
		g.latencies = append(g.latencies, b.Latencies...)
		g.s2xx += b.Status2xx
		g.s3xx += b.Status3xx
		g.s4xx += b.Status4xx
		g.s5xx += b.Status5xx
	}

	groupSlice := make([]*group, 0, len(grouped))
	for _, g := range grouped {
		groupSlice = append(groupSlice, g)
	}
	sort.Slice(groupSlice, func(i, j int) bool {
		return groupSlice[i].ts.Before(groupSlice[j].ts)
	})

	intervalSecs := rc.groupBy.Seconds()

	reqMetrics := make([]RequestMetric, 0, len(groupSlice))
	errMetrics := make([]ErrorMetric, 0, len(groupSlice))

	for _, g := range groupSlice {
		rps := float64(g.reqTotal) / intervalSecs

		var p50, p95, p99 float64
		if len(g.latencies) > 0 {
			sorted := make([]int64, len(g.latencies))
			copy(sorted, g.latencies)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			p50 = float64(sorted[int(float64(len(sorted)-1)*0.50)])
			p95 = float64(sorted[int(float64(len(sorted)-1)*0.95)])
			p99 = float64(sorted[int(float64(len(sorted)-1)*0.99)])
		}

		reqMetrics = append(reqMetrics, RequestMetric{
			Timestamp:      g.ts.UTC().Format(time.RFC3339),
			RequestsPerSec: math.Round(rps*100) / 100,
			LatencyP50:     math.Round(p50),
			LatencyP95:     math.Round(p95),
			LatencyP99:     math.Round(p99),
		})

		total := g.s2xx + g.s3xx + g.s4xx + g.s5xx
		var errRate float64
		if total > 0 {
			errRate = math.Round(float64(g.s4xx+g.s5xx)/float64(total)*10000) / 100
		}

		errMetrics = append(errMetrics, ErrorMetric{
			Timestamp: g.ts.UTC().Format(time.RFC3339),
			ErrorRate: errRate,
			Status2xx: g.s2xx,
			Status3xx: g.s3xx,
			Status4xx: g.s4xx,
			Status5xx: g.s5xx,
		})
	}

	// ── Stats ─────────────────────────────────────────────────────────────────
	total, _ := s.GetTotals()
	s2xx, s3xx, s4xx, s5xx := s.GetStatusTotals()
	allTotal := s2xx + s3xx + s4xx + s5xx

	var overallErrorRate float64
	if allTotal > 0 {
		overallErrorRate = math.Round(float64(s4xx+s5xx)/float64(allTotal)*10000) / 100
	}

	var overallAvgLatency float64
	if len(buckets) > 0 {
		var latSum, latCount int64
		for _, b := range buckets {
			latSum += b.LatencySum
			latCount += b.LatencyCount
		}
		if latCount > 0 {
			overallAvgLatency = math.Round(float64(latSum)/float64(latCount)*10) / 10
		}
	}

	stats := GatewayStats{
		TotalRequests:      total,
		TotalRequestsDelta: 0,
		AvgLatency:         overallAvgLatency,
		AvgLatencyDelta:    0,
		ErrorRate:          overallErrorRate,
		ErrorRateDelta:     0,
		ActiveRoutes:       len(modules.list()),
		Uptime:             int64(time.Since(s.StartTime()).Seconds()),
	}

	// ── Top routes ────────────────────────────────────────────────────────────
	routeStats := s.GetTopRoutes(10)
	topRoutes := make([]RouteMetric, 0, len(routeStats))
	for _, rs := range routeStats {
		var avg float64
		if rs.RequestCount > 0 {
			avg = math.Round(float64(rs.TotalLatency)/float64(rs.RequestCount)*10) / 10
		}
		var errRate float64
		if rs.RequestCount > 0 {
			errRate = math.Round(float64(rs.ErrorCount)/float64(rs.RequestCount)*10000) / 100
		}
		var p99 float64
		if len(rs.Latencies) > 0 {
			sorted := make([]int64, len(rs.Latencies))
			copy(sorted, rs.Latencies)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			p99 = float64(sorted[int(float64(len(sorted)-1)*0.99)])
		}
		topRoutes = append(topRoutes, RouteMetric{
			Route:        rs.Route,
			Method:       rs.Method,
			RequestCount: rs.RequestCount,
			AvgLatency:   avg,
			ErrorRate:    errRate,
			P99Latency:   math.Round(p99),
		})
	}

	// ── Status distribution ───────────────────────────────────────────────────
	statusDist := []StatusDist{
		{Code: "2xx", Count: s2xx},
		{Code: "3xx", Count: s3xx},
		{Code: "4xx", Count: s4xx},
		{Code: "5xx", Count: s5xx},
	}
	if allTotal > 0 {
		for i := range statusDist {
			statusDist[i].Percentage = math.Round(float64(statusDist[i].Count)/float64(allTotal)*1000) / 10
		}
	}

	return DashboardData{
		Stats:              stats,
		RequestMetrics:     reqMetrics,
		ErrorMetrics:       errMetrics,
		TopRoutes:          topRoutes,
		StatusDistribution: statusDist,
	}
}

// ─── WAF rules helpers ────────────────────────────────────────────────────────

func readWafRules(path string) ([]WafRule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f WafRuleFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return f.Rules, nil
}

func writeWafRules(path string, rules []WafRule) error {
	f := WafRuleFile{Version: 1, Rules: rules}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ─── Server startup ───────────────────────────────────────────────────────────

func StartAdminServer(cfg *config.Config, store *Store) {
	ttl := cfg.Admin.TokenTTLHours
	if ttl <= 0 {
		ttl = 12
	}
	secret := cfg.Admin.JWTSecret
	if secret == "" {
		secret = "insecure-default-secret"
	}

	modules := newModuleRegistry(cfg.Modules)

	// Resolve WAF rules path
	wafRulesPath := ""
	for _, m := range cfg.Modules {
		if m.ID == "waf" {
			if rp, ok := m.Config["rules_path"].(string); ok {
				wafRulesPath = filepath.Join("./internal/config/rules", rp)
			}
			break
		}
	}

	// Rate-limit config (in-memory, read from config)
	rlCfg := RateLimitCfg{MaxRequests: 100, WindowSeconds: 30}
	for _, m := range cfg.Modules {
		if m.ID == "ratelimit" {
			if v, ok := m.Config["max_requests"]; ok {
				switch n := v.(type) {
				case int64:
					rlCfg.MaxRequests = int(n)
				case float64:
					rlCfg.MaxRequests = int(n)
				}
			}
			if v, ok := m.Config["window_seconds"]; ok {
				switch n := v.(type) {
				case int64:
					rlCfg.WindowSeconds = int(n)
				case float64:
					rlCfg.WindowSeconds = int(n)
				}
			}
			break
		}
	}
	var rlMu sync.RWMutex

	app := fiber.New(fiber.Config{
		ServerHeader: "",
		AppName:      "",
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		},
	})

	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Origin, Content-Type, Accept, Authorization",
		AllowMethods: "GET, POST, PUT, PATCH, DELETE, OPTIONS",
	}))

	// ── Auth ──────────────────────────────────────────────────────────────────
	app.Post("/auth/login", func(c *fiber.Ctx) error {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
		}
		if body.Username != cfg.Admin.Username || !checkPassword(cfg.Admin.PasswordHash, body.Password) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid credentials"})
		}
		token, err := generateToken(body.Username, secret, ttl)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token generation failed"})
		}
		return c.JSON(fiber.Map{
			"token":   token,
			"ttl_hours": ttl,
			"username": body.Username,
		})
	})

	// ── Protected API ─────────────────────────────────────────────────────────
	api := app.Group("/api", jwtMiddleware(secret))

	// Health
	api.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status": "ok",
			"uptime": int64(time.Since(store.StartTime()).Seconds()),
		})
	})

	// Metrics / dashboard
	api.Get("/metrics", func(c *fiber.Ctx) error {
		rangeStr := c.Query("range", "24h")
		data := buildDashboard(store, modules, rangeStr)
		return c.JSON(data)
	})

	// Logs
	api.Get("/logs", func(c *fiber.Ctx) error {
		filter := c.Query("filter", "all")
		limit := c.QueryInt("limit", 100)
		if limit > 1000 {
			limit = 1000
		}
		logs := store.GetLogs(filter, limit)
		return c.JSON(logs)
	})

	// WAF rules
	api.Get("/waf/rules", func(c *fiber.Ctx) error {
		if wafRulesPath == "" {
			return c.JSON([]WafRule{})
		}
		rules, err := readWafRules(wafRulesPath)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(rules)
	})

	api.Put("/waf/rules", func(c *fiber.Ctx) error {
		if wafRulesPath == "" {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "waf rules path not configured"})
		}
		var rules []WafRule
		if err := c.BodyParser(&rules); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
		}
		if err := writeWafRules(wafRulesPath, rules); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"ok": true, "count": len(rules)})
	})

	// Modules
	api.Get("/modules", func(c *fiber.Ctx) error {
		return c.JSON(modules.list())
	})

	api.Patch("/modules/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
		}
		m, ok := modules.patch(id, body.Enabled)
		if !ok {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "module not found"})
		}
		return c.JSON(m)
	})

	// Rate-limit config
	api.Get("/rate-limit", func(c *fiber.Ctx) error {
		rlMu.RLock()
		defer rlMu.RUnlock()
		return c.JSON(rlCfg)
	})

	api.Put("/rate-limit", func(c *fiber.Ctx) error {
		var body RateLimitCfg
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
		}
		if body.MaxRequests <= 0 || body.WindowSeconds <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "max_requests and window_seconds must be > 0"})
		}
		rlMu.Lock()
		rlCfg = body
		rlMu.Unlock()
		return c.JSON(fiber.Map{"ok": true, "note": "restart required to apply rate-limit changes to WASM module"})
	})

	bind := cfg.Server.AdminBind
	if bind == "" {
		bind = "0.0.0.0:9090"
	}

	utils.LogSuccess("Admin API listening on %s", bind)
	if err := app.Listen(bind); err != nil {
		utils.LogWarning("Admin server stopped: %v", err)
	}
}
