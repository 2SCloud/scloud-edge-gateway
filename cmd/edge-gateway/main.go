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
	"2scloud-edge-gateway/internal/utils"

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
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Request allowed by WAF")
	})

	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/rate-limit" {
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
			return
		}

		callCtx := ctx
		if wafCfg.TimeoutMs > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, time.Duration(wafCfg.TimeoutMs)*time.Millisecond)
			defer cancel()
		}

		reqObj := runtime.BuildWafRequest(req)
		decision, err := runtime.CallModule(callCtx, wafModule, wafCfg, reqObj)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reason, _ := runtime.ReadLastReason(callCtx, wafModule)
		wafHeader := "processed"
		if decision == 0 && reason == "scope-bypass" {
			wafHeader = "bypass"
		}
		w.Header().Set("SCLOUD-X-WAF", wafHeader)

		utils.LogInfo("WAF %s: method=%s path=%s decision=%d reason=%s", wafHeader, req.Method, req.URL.Path, decision, reason)

		if decision != 0 {
			w.WriteHeader(http.StatusForbidden)

			fmt.Fprintf(w, "Request blocked by WAF\n\treason=%s\n\tmethod=%s\n\tpath=%s\nprotected by 2SCloud\n", reason, req.Method, req.URL.Path)
			utils.LogWarning("WAF BLOCK: reason=%q method=%s path=%s", reason, req.Method, req.URL.Path)
			return
		} else {
			mux.ServeHTTP(w, req)
		}

	})

	srv := &http.Server{
		Addr:    cfg.Server.Bind,
		Handler: rootHandler,
	}

	utils.LogDebug("Edge Gateway version: %s", cfg.Version)
	utils.LogSuccess("Edge Gateway listening on %s (config: %s)", cfg.Server.Bind, configPath)
	log.Fatal(srv.ListenAndServe())

}
