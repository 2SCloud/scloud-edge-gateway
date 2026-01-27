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

// /////////////////////
// GENERIC functions for WASM runtimes
// /////////////////////
func FindModule(cfg *config.Config, id string) *config.ModuleCfg {
	for i := range cfg.Modules {
		if cfg.Modules[i].ID == id {
			return &cfg.Modules[i]
		}
	}
	return nil
}

func callWithBytes(ctx context.Context, m api.Module, allocName, fnName string, payload []byte) (uint64, error) {
	fn := m.ExportedFunction(fnName)
	if fn == nil {
		return 0, fmt.Errorf("%s function not found", fnName)
	}
	alloc := m.ExportedFunction(allocName)
	if alloc == nil {
		return 0, fmt.Errorf("%s function not found", allocName)
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

func InitModule(ctx context.Context, mod api.Module, mc *config.ModuleCfg) error {
	if mc == nil || mc.Init == "" {
		return nil
	}
	if mc.Alloc == "" {
		return fmt.Errorf("module %q: alloc export name missing", mc.ID)
	}

	payload, err := json.Marshal(mc.Config)
	if err != nil {
		return fmt.Errorf("module %q: config marshal failed: %w", mc.ID, err)
	}

	rc, err := callWithBytes(ctx, mod, mc.Alloc, mc.Init, payload)
	if err != nil {
		return fmt.Errorf("module %q: init failed: %w", mc.ID, err)
	}
	if rc != 0 {
		return fmt.Errorf("module %q: init returned non-zero: %d", mc.ID, rc)
	}
	return nil
}

///////////////////////
// END OF GENERIC functions for WASM runtimes
///////////////////////

func LoadWasmModule(ctx context.Context, r wazero.Runtime, wasmBytes []byte) (api.Module, error) {
	compiledModule, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to compile WASM module: %w", err)
	}

	module, err := r.InstantiateModule(ctx, compiledModule, wazero.NewModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate WASM module: %w", err)
	}

	return module, nil
}

func CallWaf(ctx context.Context, module api.Module, _request []byte) (int64, error) {
	handle := module.ExportedFunction("handle")
	if handle == nil {
		return -1, fmt.Errorf("handle function not found in WASM module")
	}

	results, err := handle.Call(ctx, 0, 0)
	if err != nil {
		return -1, fmt.Errorf("failed to call WASM function: %w", err)
	}

	return int64(results[0]), nil
}

func CallRatelimit(ctx context.Context, module api.Module, req http.Request) (int64, error) {
	handle := module.ExportedFunction("handle")
	if handle == nil {
		return -1, fmt.Errorf("handle function not found")
	}

	alloc := module.ExportedFunction("alloc")
	if alloc == nil {
		return -1, fmt.Errorf("alloc function not found")
	}

	mem := module.Memory()
	if mem == nil {
		return -1, fmt.Errorf("WASM memory not found")
	}

	addrBytes := []byte(req.RemoteAddr)

	ptrResults, err := alloc.Call(ctx, uint64(len(addrBytes)))
	if err != nil {
		return -1, fmt.Errorf("alloc failed: %w", err)
	}

	ptr := uint32(ptrResults[0])

	if !mem.Write(ptr, addrBytes) {
		return -1, fmt.Errorf("failed to write WASM memory")
	}

	results, err := handle.Call(
		ctx,
		uint64(ptr),
		uint64(len(addrBytes)),
	)
	if err != nil {
		return -1, fmt.Errorf("handle call failed: %w", err)
	}

	return int64(results[0]), nil
}
