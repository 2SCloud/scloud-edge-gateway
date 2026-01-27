package runtime

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

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
