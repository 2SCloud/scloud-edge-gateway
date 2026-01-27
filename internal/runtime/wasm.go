package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"2scloud-edge-gateway/internal/config"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// =======================
// Generic helpers
// =======================

func FindModule(cfg *config.Config, id string) *config.ModuleCfg {
	for i := range cfg.Modules {
		if cfg.Modules[i].ID == id {
			return &cfg.Modules[i]
		}
	}
	return nil
}

func resolveFns(mc *config.ModuleCfg) (entryFn, allocFn, initFn string) {
	entryFn = mc.EntryFn
	if entryFn == "" {
		entryFn = "handle"
	}
	allocFn = mc.AllocFn
	if allocFn == "" {
		allocFn = "alloc"
	}
	initFn = mc.InitFn
	if initFn == "" {
		initFn = "init"
	}
	return
}

func callWithBytes(ctx context.Context, m api.Module, allocFn, fnName string, payload []byte) (uint64, error) {
	fn := m.ExportedFunction(fnName)
	if fn == nil {
		return 0, fmt.Errorf("%s function not found", fnName)
	}
	alloc := m.ExportedFunction(allocFn)
	if alloc == nil {
		return 0, fmt.Errorf("%s function not found", allocFn)
	}
	mem := m.Memory()
	if mem == nil {
		return 0, fmt.Errorf("WASM memory not found")
	}

	ptrRes, err := alloc.Call(ctx, uint64(len(payload)))
	if err != nil {
		return 0, fmt.Errorf("alloc failed: %w", err)
	}
	ptr := uint32(ptrRes[0])

	if !mem.Write(ptr, payload) {
		return 0, fmt.Errorf("failed to write WASM memory")
	}

	res, err := fn.Call(ctx, uint64(ptr), uint64(len(payload)))
	if err != nil {
		return 0, fmt.Errorf("%s call failed: %w", fnName, err)
	}
	return res[0], nil
}

// =======================
// Module lifecycle
// =======================

func LoadWasmModule(ctx context.Context, r wazero.Runtime, wasmBytes []byte) (api.Module, error) {
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile wasm failed: %w", err)
	}

	module, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("instantiate wasm failed: %w", err)
	}

	return module, nil
}

// init(config) uniquement si config_mode=init
func InitModule(ctx context.Context, mod api.Module, mc *config.ModuleCfg) error {
	if mc.ConfigMode != "init" {
		return nil
	}

	entryFn, allocFn, initFn := resolveFns(mc)

	if mod.ExportedFunction(initFn) == nil {
		return fmt.Errorf(
			"module %q: config_mode=init but function %q not found",
			mc.ID, initFn,
		)
	}

	payload, err := json.Marshal(mc.Config)
	if err != nil {
		return fmt.Errorf("module %q: config marshal failed: %w", mc.ID, err)
	}

	rc, err := callWithBytes(ctx, mod, allocFn, initFn, payload)
	if err != nil {
		return fmt.Errorf("module %q: init failed: %w", mc.ID, err)
	}
	if rc != 0 {
		return fmt.Errorf("module %q: init returned %d", mc.ID, rc)
	}

	_ = entryFn // silence lint, entryFn used later
	return nil
}

// =======================
// Unified call (init / inline)
// =======================

func CallModule(
	ctx context.Context,
	mod api.Module,
	mc *config.ModuleCfg,
	request map[string]any,
) (int64, error) {

	entryFn, allocFn, _ := resolveFns(mc)

	var payload any

	switch mc.ConfigMode {
	case "init":
		// handle(request)
		payload = request

	case "inline":
		// handle({config, request})
		payload = map[string]any{
			"config":  mc.Config,
			"request": request,
		}

	default:
		return -1, fmt.Errorf("module %q: unknown config_mode %q", mc.ID, mc.ConfigMode)
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return -1, fmt.Errorf("marshal payload failed: %w", err)
	}

	out, err := callWithBytes(ctx, mod, allocFn, entryFn, b)
	if err != nil {
		return -1, err
	}

	return int64(out), nil
}

// =======================
// Request builders
// =======================

func BuildWafRequest(req *http.Request) map[string]any {
	return map[string]any{
		"path":   req.URL.Path,
		"method": req.Method,
	}
}

func BuildRateLimitRequest(req *http.Request) map[string]any {
	return map[string]any{
		"ip":     req.RemoteAddr,
		"path":   req.URL.Path,
		"method": req.Method,
	}
}
