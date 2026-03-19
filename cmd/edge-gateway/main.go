package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"2scloud-edge-gateway/internal/config"
	"2scloud-edge-gateway/internal/runtime"
	"2scloud-edge-gateway/internal/utils"

	"github.com/BurntSushi/toml"
	"github.com/gofiber/fiber/v2"
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
	// Fiber app
	// ========================
	app := fiber.New(fiber.Config{
		// Masque "Fiber" dans les headers
		ServerHeader: "",
		AppName:      "",
		// Erreurs JSON cohérentes
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		},
	})

	// ── Middleware WAF (toutes les routes sauf /healthz) ─────────────────────
	app.Use(wafMiddleware(ctx, wafMod, wafCfg))

	// ── Routes ───────────────────────────────────────────────────────────────
	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	app.All("/rate-limit", rateLimitHandler(ctx, rlMod, rlCfg))

	app.All("/", func(c *fiber.Ctx) error {
		return c.SendString("Request allowed by WAF")
	})

	// ── Start ─────────────────────────────────────────────────────────────────
	utils.LogDebug("Edge Gateway version: %s", cfg.Version)
	utils.LogSuccess("Edge Gateway listening on %s (config: %s)", cfg.Server.Bind, configPath)
	log.Fatal(app.Listen(cfg.Server.Bind))
}

// ─────────────────────────────────────────────────────────────────────────────
// loadAndInit charge le .wasm depuis le disque et l'initialise.
// Retourne le module WASM (api.Module) ET sa config (pour TimeoutMs, ConfigMode…).
// Toutes les erreurs sont retournées — aucune ignorée silencieusement.
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
func wafMiddleware(baseCtx context.Context, mod api.Module, modCfg *config.ModuleCfg) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// /healthz ne passe pas par le WAF
		if c.Path() == "/healthz" {
			return c.Next()
		}

		callCtx := withCallTimeout(baseCtx, modCfg.TimeoutMs)
		defer callCtx.cancel()

		reqObj := runtime.BuildWafRequestFasthttp(c.Request())
		decision, err := runtime.CallModule(callCtx, mod, modCfg, reqObj)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}

		reason, _ := runtime.ReadLastReason(callCtx, mod)

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
			return c.Status(fiber.StatusForbidden).SendString(
				fmt.Sprintf("Request blocked by WAF\n\treason=%s\n\tmethod=%s\n\tpath=%s\nprotected by 2SCloud\n",
					reason, c.Method(), c.Path()),
			)
		}

		return c.Next()
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

// withCallTimeout retourne un contexte annulable avec timeout optionnel.
// Appeler .cancel() libère les ressources immédiatement après l'appel WASM.
func withCallTimeout(parent context.Context, timeoutMs int) cancelCtx {
	if timeoutMs <= 0 {
		// Pas de deadline : cancel no-op pour que l'appelant puisse toujours defer .cancel()
		ctx, cancel := context.WithCancel(parent)
		return cancelCtx{ctx, cancel}
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutMs)*time.Millisecond)
	return cancelCtx{ctx, cancel}
}
