package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"2scloud-edge-gateway/internal/admin"
	"2scloud-edge-gateway/internal/config"
	"2scloud-edge-gateway/internal/proxy"
	"2scloud-edge-gateway/internal/runtime"
	"2scloud-edge-gateway/internal/utils"

	"github.com/BurntSushi/toml"
	"github.com/gofiber/fiber/v2"
	fiberproxy "github.com/gofiber/fiber/v2/middleware/proxy"
	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func main() {
	ctx := context.Background()

	// ========================
	// Load config
	// ========================
	configPath := os.Getenv("EDGE_CONFIG")
	if configPath == "" {
		configPath = "./internal/config/config.toml"
	}

	var cfg config.Config
	if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// ========================
	// Shared admin store
	// ========================
	store := admin.NewStore()

	// ========================
	// WASM runtime
	// ========================
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		log.Fatalf("failed to instantiate WASI: %v", err)
	}

	// ========================
	// Load + init modules
	// ========================
	wafMod, wafCfg, err := loadAndInit(ctx, r, &cfg, "waf")
	if err != nil {
		log.Fatalf("waf: %v", err)
	}

	rlMod, rlCfg, err := loadAndInit(ctx, r, &cfg, "ratelimit")
	if err != nil {
		log.Fatalf("ratelimit: %v", err)
	}

	// ========================
	// Launch admin server
	// ========================
	go admin.StartAdminServer(&cfg, store)

	// ========================
	// Fiber app
	// ========================
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

	// ── Middleware WAF (toutes les routes sauf /healthz) ─────────────────────
	app.Use(wafMiddleware(ctx, wafMod, wafCfg, store))

	// ── Routes ───────────────────────────────────────────────────────────────
	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	app.All("/rate-limit", rateLimitHandler(ctx, rlMod, rlCfg))

	// DoH (RFC 8484). Prefer the HTTP passthrough to scloud-dns's native
	// DoH endpoint; fall back to the in-gateway DoH→UDP translator
	// (internal/proxy) if only [dns] is configured. Every /dns-query
	// request still passes through the WAF middleware above, so malformed
	// or blocked queries are logged to the admin store.
	switch {
	case cfg.DoH.Enabled && cfg.DoH.UpstreamURL != "":
		paths := cfg.DoH.Paths
		if len(paths) == 0 {
			paths = []string{"/dns-query"}
		}
		for _, p := range paths {
			target := strings.TrimRight(cfg.DoH.UpstreamURL, "/") + p
			utils.LogInfo("DoH route %s -> %s (http passthrough)", p, target)
			app.All(p, dohProxyHandler(target))
		}
	case cfg.DNS.UpstreamAddr != "":
		doh, err := proxy.New(proxy.Config{
			UpstreamAddr:   cfg.DNS.UpstreamAddr,
			QueryTimeout:   time.Duration(cfg.DNS.QueryTimeoutMs) * time.Millisecond,
			MaxMessageSize: cfg.DNS.MaxMessageSize,
		})
		if err != nil {
			log.Fatalf("doh: %v", err)
		}
		app.All("/dns-query", doh.Handler())
		utils.LogSuccess("DoH endpoint enabled on /dns-query → %s (udp translator)", cfg.DNS.UpstreamAddr)
	default:
		utils.LogInfo("DoH disabled (set [doh].upstream_url or [dns].upstream_addr)")
	}

	app.All("/", func(c *fiber.Ctx) error {
		return c.SendString("Request allowed by WAF")
	})

	// ── Start ─────────────────────────────────────────────────────────────────
	utils.LogDebug("Edge Gateway version: %s", cfg.Version)

	// TLS listener (public-facing). When tls_bind + cert/key are all
	// configured we serve HTTPS alongside the plain-HTTP listener.
	// Plain HTTP is needed unconditionally for kubelet liveness probes
	// against /healthz — the kubelet can't validate an internal CA.
	//
	// A TLS failure (missing cert, bad key, port conflict) must not
	// kill the process: the plain listener has to keep serving so the
	// pod stays Ready and the WAF/admin API stay reachable. We log
	// loudly and move on.
	if cfg.Server.TLSBind != "" && cfg.Server.TLSCertFile != "" && cfg.Server.TLSKeyFile != "" {
		if err := assertReadable(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile); err != nil {
			utils.LogWarning("TLS listener disabled: %v", err)
		} else {
			go func() {
				utils.LogSuccess("Edge Gateway (TLS) listening on %s (cert: %s)",
					cfg.Server.TLSBind, cfg.Server.TLSCertFile)
				if err := app.ListenTLS(cfg.Server.TLSBind, cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile); err != nil {
					utils.LogError("TLS listener stopped: %v (plain HTTP listener still running)", err)
				}
			}()
		}
	}

	utils.LogSuccess("Edge Gateway listening on %s (config: %s)", cfg.Server.Bind, configPath)
	log.Fatal(app.Listen(cfg.Server.Bind))
}

// assertReadable fails fast when any of the given paths cannot be
// stat'd or read. Used as a pre-flight for ListenTLS so we can
// disable TLS cleanly instead of crashing the whole gateway when
// the cert secret hasn't been mounted yet.
func assertReadable(paths ...string) error {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("cannot open %s: %w", p, err)
		}
		_ = f.Close()
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// loadAndInit charge le .wasm depuis le disque et l'initialise.
// ─────────────────────────────────────────────────────────────────────────────
func loadAndInit(ctx context.Context, r wazero.Runtime, cfg *config.Config, name string) (api.Module, *config.ModuleCfg, error) {
	modCfg := runtime.FindModule(cfg, name)
	if modCfg == nil {
		return nil, nil, fmt.Errorf("module %q not found in config", name)
	}
	if !modCfg.Enabled {
		return nil, nil, fmt.Errorf("module %q is disabled", name)
	}

	wasmBytes, err := os.ReadFile(modCfg.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %q wasm (%s): %w", name, modCfg.Path, err)
	}

	mod, err := runtime.LoadWasmModule(ctx, r, wasmBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("load %q wasm: %w", name, err)
	}

	if err := runtime.InitModule(ctx, mod, modCfg); err != nil {
		return nil, nil, fmt.Errorf("init %q module: %w", name, err)
	}

	return mod, modCfg, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// wafMiddleware inspecte chaque requête via le module WASM WAF.
// ─────────────────────────────────────────────────────────────────────────────
func wafMiddleware(baseCtx context.Context, mod api.Module, modCfg *config.ModuleCfg, store *admin.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if c.Path() == "/healthz" {
			return c.Next()
		}

		start := time.Now()

		callCtx := withCallTimeout(baseCtx, modCfg.TimeoutMs)
		defer callCtx.cancel()

		reqObj := runtime.BuildWafRequestFasthttp(c.Request())
		decision, err := runtime.CallModule(callCtx, mod, modCfg, reqObj)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}

		reason, _ := runtime.ReadLastReason(callCtx, mod)
		latencyMs := time.Since(start).Milliseconds()

		wafHeader := "processed"
		if decision == 0 && reason == "scope-bypass" {
			wafHeader = "bypass"
		}
		c.Set("SCLOUD-X-WAF", wafHeader)

		utils.LogInfo("WAF %s: method=%s path=%s decision=%d reason=%s",
			wafHeader, c.Method(), c.Path(), decision, reason)

		if decision != 0 {
			utils.LogWarning("WAF BLOCK: reason=%q method=%s path=%s",
				reason, c.Method(), c.Path())

			store.AddLog(admin.LogEntry{
				ID:         uuid.New().String(),
				Timestamp:  time.Now(),
				IP:         c.IP(),
				Method:     c.Method(),
				Path:       c.Path(),
				Decision:   "block",
				Reason:     reason,
				StatusCode: fiber.StatusForbidden,
				LatencyMs:  latencyMs,
			})

			return c.Status(fiber.StatusForbidden).SendString(
				fmt.Sprintf("Request blocked by WAF\n\treason=%s\n\tmethod=%s\n\tpath=%s\nprotected by 2SCloud\n",
					reason, c.Method(), c.Path()),
			)
		}

		// For allowed requests, record after the full handler runs via defer
		// so we capture the actual status code
		defer func() {
			store.AddLog(admin.LogEntry{
				ID:         uuid.New().String(),
				Timestamp:  time.Now(),
				IP:         c.IP(),
				Method:     c.Method(),
				Path:       c.Path(),
				Decision:   "allow",
				Reason:     reason,
				StatusCode: c.Response().StatusCode(),
				LatencyMs:  time.Since(start).Milliseconds(),
			})
		}()

		return c.Next()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// dohProxyHandler forwards RFC 8484 DoH requests to scloud-dns, preserving
// method, body and the application/dns-message Content-Type.
// ─────────────────────────────────────────────────────────────────────────────
func dohProxyHandler(target string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if err := fiberproxy.Do(c, target); err != nil {
			return fiber.NewError(fiber.StatusBadGateway, err.Error())
		}
		c.Response().Header.Del(fiber.HeaderServer)
		return nil
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// rateLimitHandler appelle le module WASM rate-limit.
// ─────────────────────────────────────────────────────────────────────────────
func rateLimitHandler(baseCtx context.Context, mod api.Module, modCfg *config.ModuleCfg) fiber.Handler {
	return func(c *fiber.Ctx) error {
		callCtx := withCallTimeout(baseCtx, modCfg.TimeoutMs)
		defer callCtx.cancel()

		reqObj := runtime.BuildRateLimitRequestFasthttp(c.Request())
		decision, err := runtime.CallModule(callCtx, mod, modCfg, reqObj)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}

		if decision == 0 {
			return c.SendString("RateLimit OK")
		}
		return c.Status(fiber.StatusForbidden).SendString("Request blocked by RateLimit")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers context
// ─────────────────────────────────────────────────────────────────────────────

type cancelCtx struct {
	context.Context
	cancel context.CancelFunc
}

func withCallTimeout(parent context.Context, timeoutMs int) cancelCtx {
	if timeoutMs <= 0 {
		ctx, cancel := context.WithCancel(parent)
		return cancelCtx{ctx, cancel}
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutMs)*time.Millisecond)
	return cancelCtx{ctx, cancel}
}
