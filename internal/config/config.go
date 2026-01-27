package config

type Config struct {
	Version string      `toml:"version"`
	Server  ServerCfg   `toml:"server"`
	Logging LoggingCfg  `toml:"logging"`
	Wasm    WasmCfg     `toml:"wasm"`
	Modules []ModuleCfg `toml:"modules"`
}

type ServerCfg struct {
	Bind               string `toml:"bind"`
	AdminBind          string `toml:"admin_bind"`
	GracefulShutdownMs int    `toml:"graceful_shutdown_ms"`
}

type LoggingCfg struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
	Output string `toml:"output"`
}

type WasmCfg struct {
	Runtime        string `toml:"runtime"`
	CacheDir       string `toml:"cache_dir"`
	MaxModules     int    `toml:"max_modules"`
	Fuel           uint64 `toml:"fuel"`
	MaxMemoryMB    int    `toml:"max_memory_mb"`
	MaxStackKB     int    `toml:"max_stack_kb"`
	MaxMemoryPages uint32 `toml:"max_memory_pages"`
}

type ModuleCfg struct {
	ID         string         `toml:"id"`
	Path       string         `toml:"path"`
	Enabled    bool           `toml:"enabled"`
	Entrypoint string         `toml:"entrypoint"`
	Alloc      string         `toml:"alloc"`
	Init       string         `toml:"init"`
	Route      string         `toml:"route"`
	TimeoutMs  int            `toml:"timeout_ms"`
	Config     map[string]any `toml:"config"`
}
