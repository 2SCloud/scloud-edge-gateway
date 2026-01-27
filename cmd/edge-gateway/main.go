package main

import (
	"context"
	"encoding/json"
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
	// Load TOML config
	// ========================
	configPath := os.Getenv("EDGE_CONFIG")
	if configPath == "" {
		configPath = "./internal/config/config.toml"
	}

	var cfg config.Config
	if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
		log.Fatalf("failed to read config %s: %v", configPath, err)
	}
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = ":8080"
	}

	// ========================
	// WASM runtime (wazero)
	// ========================
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		log.Fatalf("failed to instantiate WASI: %v", err)
	}

	// ========================
	// Load & init modules
	// ========================
	wafCfg := runtime.FindModule(&cfg, "waf")
	rlCfg := runtime.FindModule(&cfg, "ratelimit")

	if wafCfg == nil || !wafCfg.Enabled {
		log.Fatalf("waf module missing or disabled in config")
	}
	if rlCfg == nil || !rlCfg.Enabled {
		log.Fatalf("ratelimit module missing or disabled in config")
	}

	if wafCfg.Entrypoint == "" {
		wafCfg.Entrypoint = "handle"
	}
	if wafCfg.Alloc == "" {
		wafCfg.Alloc = "alloc"
	}

	if rlCfg.Entrypoint == "" {
		rlCfg.Entrypoint = "handle"
	}
	if rlCfg.Alloc == "" {
		rlCfg.Alloc = "alloc"
	}

	wafBytes, err := os.ReadFile(wafCfg.Path)
	if err != nil {
		log.Fatalf("failed to read waf wasm file %s: %v", wafCfg.Path, err)
	}
	rlBytes, err := os.ReadFile(rlCfg.Path)
	if err != nil {
		log.Fatalf("failed to read ratelimit wasm file %s: %v", rlCfg.Path, err)
	}

	wafModule, err := runtime.LoadWasmModule(ctx, r, wafBytes)
	if err != nil {
		log.Fatalf("failed to load waf module: %v", err)
	}
	rlModule, err := runtime.LoadWasmModule(ctx, r, rlBytes)
	if err != nil {
		log.Fatalf("failed to load ratelimit module: %v", err)
	}

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
		requestData := map[string]string{
			"path":   req.URL.Path,
			"method": req.Method,
		}
		jsonData, _ := json.Marshal(requestData)

		callCtx := ctx
		if wafCfg.TimeoutMs > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, time.Duration(wafCfg.TimeoutMs)*time.Millisecond)
			defer cancel()
		}

		decision, err := runtime.CallWaf(callCtx, wafModule, jsonData)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "WAF error: %v", err)
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

		decision, err := runtime.CallRatelimit(callCtx, rlModule, *req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "RateLimit error: %v", err)
			return
		}

		if decision == 0 {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "RateLimit OK")
		} else if decision == 1 {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(w, "Request blocked by RateLimit")
		} else {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "RateLimit decision=%d\n", decision)
		}
	})

	log.Printf("Edge Gateway listening on %s (config: %s)", cfg.Server.Bind, configPath)
	log.Fatal(http.ListenAndServe(cfg.Server.Bind, nil))
}
