package config

type Config struct {
	Version string      `toml:"version"`
	Server  ServerCfg   `toml:"server"`
	Admin   AdminCfg    `toml:"admin"`
	Logging LoggingCfg  `toml:"logging"`
	Wasm    WasmCfg     `toml:"wasm"`
	Modules []ModuleCfg `toml:"modules"`
}

type ServerCfg struct {
	Bind               string `toml:"bind"`
	AdminBind          string `toml:"admin_bind"`
	GracefulShutdownMs int    `toml:"graceful_shutdown_ms"`
}

type AdminCfg struct {
	Username      string `toml:"username"`
	PasswordHash  string `toml:"password_hash"` // bcrypt hash or plaintext (dev only)
	JWTSecret     string `toml:"jwt_secret"`
	TokenTTLHours int    `toml:"token_ttl_hours"`
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
	ID         string `toml:"id"`
	Path       string `toml:"path"`
	Enabled    bool   `toml:"enabled"`
	Route      string `toml:"route"`
	TimeoutMs  int    `toml:"timeout_ms"`
	ConfigMode string `toml:"config_mode"` // "init" | "inline"

	EntryFn string         `toml:"entry_fn"`
	AllocFn string         `toml:"alloc_fn"`
	InitFn  string         `toml:"init_fn"`
	Config  map[string]any `toml:"config"`
}
