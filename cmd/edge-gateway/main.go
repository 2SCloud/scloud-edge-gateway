package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"2scloud-edge-gateway/internal/config"
	"2scloud-edge-gateway/internal/runtime"

	"github.com/BurntSushi/toml"
	"github.com/tetratelabs/wazero"
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
	// Load modules
	// ========================
	wafCfg := runtime.FindModule(&cfg, "waf")
	rlCfg := runtime.FindModule(&cfg, "ratelimit")

	if wafCfg == nil || !wafCfg.Enabled {
		log.Fatal("waf module missing or disabled")
	}
	if rlCfg == nil || !rlCfg.Enabled {
		log.Fatal("ratelimit module missing or disabled")
	}

	wafBytes, _ := os.ReadFile(wafCfg.Path)
	rlBytes, _ := os.ReadFile(rlCfg.Path)

	wafModule, err := runtime.LoadWasmModule(ctx, r, wafBytes)
	if err != nil {
		log.Fatalf("load waf failed: %v", err)
	}
	rlModule, err := runtime.LoadWasmModule(ctx, r, rlBytes)
	if err != nil {
		log.Fatalf("load ratelimit failed: %v", err)
	}

	// ========================
	// Init modules (config_mode=init)
	// ========================
	if err := runtime.InitModule(ctx, wafModule, wafCfg); err != nil {
		log.Fatalf("waf init error: %v", err)
	}
	if err := runtime.InitModule(ctx, rlModule, rlCfg); err != nil {
		log.Fatalf("ratelimit init error: %v", err)
	}

	// ========================
	// HTTP handlers
	// ========================
	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		callCtx := ctx
		if wafCfg.TimeoutMs > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, time.Duration(wafCfg.TimeoutMs)*time.Millisecond)
			defer cancel()
		}

		reqObj := runtime.BuildWafRequest(req)
		decision, err := runtime.CallModule(callCtx, wafModule, wafCfg, reqObj)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		if decision == 0 {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "Request allowed by WAF")
		} else {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(w, "Request blocked by WAF")
		}
	})

	http.HandleFunc("/rate-limit", func(w http.ResponseWriter, req *http.Request) {
		callCtx := ctx
		if rlCfg.TimeoutMs > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, time.Duration(rlCfg.TimeoutMs)*time.Millisecond)
			defer cancel()
		}

		reqObj := runtime.BuildRateLimitRequest(req)
		decision, err := runtime.CallModule(callCtx, rlModule, rlCfg, reqObj)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		if decision == 0 {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "RateLimit OK")
		} else {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(w, "Request blocked by RateLimit")
		}
	})

	log.Printf("Edge Gateway listening on %s (config: %s)", cfg.Server.Bind, configPath)
	log.Fatal(http.ListenAndServe(cfg.Server.Bind, nil))
}
