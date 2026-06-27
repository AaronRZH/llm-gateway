package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 全局配置
type Config struct {
	App             AppConfig             `mapstructure:"app"`
	Log             LogConfig             `mapstructure:"log"`
	Redis           RedisConfig           `mapstructure:"redis"`
	ModelMapping    ModelMappingConfig    `mapstructure:"model_mapping"`
	ModelGroups     map[string]ModelGroup `mapstructure:"model_groups"`
	CircuitBreaker  CircuitBreakerConfig  `mapstructure:"circuit_breaker"`
	RateLimit       RateLimitConfig       `mapstructure:"rate_limit"`
	Providers       map[string]ProviderConfig `mapstructure:"providers"`
	Token           TokenConfig           `mapstructure:"token"`
	Metrics         MetricsConfig         `mapstructure:"metrics"`
	Health          HealthConfig          `mapstructure:"health"`
	APIKeys          []APIKeyConfig        `mapstructure:"api_keys"`
}

type AppConfig struct {
	Name    string `mapstructure:"name"`
	Version string `mapstructure:"version"`
	Env     string `mapstructure:"env"`
	Port    int    `mapstructure:"port"`
}

type LogConfig struct {
	Level   string `mapstructure:"level"`
	Format  string `mapstructure:"format"`
	Output  string `mapstructure:"output"`
}

type RedisConfig struct {
	Addr         string        `mapstructure:"addr"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"`
	PoolSize     int           `mapstructure:"pool_size"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type ModelMappingConfig struct {
	VirtualToReal map[string]VirtualModel `mapstructure:"virtual_to_real"`
	RealToVirtual map[string]string       `mapstructure:"real_to_virtual"`
}

type VirtualModel struct {
	Real      string   `mapstructure:"real"`
	Aliases   []string `mapstructure:"aliases"`
}

type ModelGroup struct {
	Strategy       string          `mapstructure:"strategy"`
	FallbackChain  []FallbackItem  `mapstructure:"fallback_chain"`
}

type FallbackItem struct {
	Provider string        `mapstructure:"provider"`
	Model    string        `mapstructure:"model"`
	Weight   int           `mapstructure:"weight"`
	Timeout  time.Duration `mapstructure:"timeout"`
	Retry    int           `mapstructure:"retry"`
}

type CircuitBreakerConfig struct {
	MaxRequests       uint          `mapstructure:"max_requests"`
	Interval          time.Duration `mapstructure:"interval"`
	Timeout           time.Duration `mapstructure:"timeout"`
	FailureThreshold  int           `mapstructure:"failure_threshold"`
	Cooldown          time.Duration `mapstructure:"cooldown"`
}

type RateLimitConfig struct {
	Enabled             bool  `mapstructure:"enabled"`
	RequestsPerSecond   int   `mapstructure:"requests_per_second"`
	Burst               int   `mapstructure:"burst"`
}

type ProviderConfig struct {
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"`
	Timeout time.Duration `mapstructure:"timeout"`
}

type TokenConfig struct {
	TokenizerMapping map[string]string `mapstructure:"tokenizer_mapping"`
	OfficialSync     OfficialSyncConfig `mapstructure:"official_sync"`
}

type OfficialSyncConfig struct {
	Enabled    bool          `mapstructure:"enabled"`
	Interval   time.Duration `mapstructure:"interval"`
	BatchSize  int           `mapstructure:"batch_size"`
}

type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
	Port    int    `mapstructure:"port"`
}

type HealthConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
}

type APIKeyConfig struct {
	Key  string `mapstructure:"key"`
	Name string `mapstructure:"name"`
}

// Load 加载配置
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	// 环境变量支持
	v.SetEnvPrefix("LLM")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 默认值
	v.SetDefault("app.port", 8080)
	v.SetDefault("log.level", "info")
	v.SetDefault("redis.addr", "localhost:6379")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config failed: %w", err)
	}

	// 加载环境特定配置（如 config.prod.yaml）
	env := v.GetString("app.env")
	if env != "" && env != "dev" {
		envConfig := fmt.Sprintf("configs/config.%s.yaml", env)
		if _, err := os.Stat(envConfig); err == nil {
			v.SetConfigFile(envConfig)
			if err := v.MergeInConfig(); err != nil {
				return nil, fmt.Errorf("merge env config failed: %w", err)
			}
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config failed: %w", err)
	}

	// 解析环境变量中的 API Key（如 ${OPENAI_API_KEY}）
	for name, p := range cfg.Providers {
		p.APIKey = resolveEnv(p.APIKey)
		cfg.Providers[name] = p
	}

	return &cfg, nil
}

// resolveEnv 解析 ${ENV_VAR} 格式的环境变量
func resolveEnv(value string) string {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		envVar := value[2 : len(value)-1]
		if v := os.Getenv(envVar); v != "" {
			return v
		}
	}
	return value
}
