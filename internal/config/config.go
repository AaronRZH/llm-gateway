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
	App            AppConfig                 `mapstructure:"app"`
	Log            LogConfig                 `mapstructure:"log"`
	Redis          RedisConfig               `mapstructure:"redis"`
	Postgres       PostgresConfig            `mapstructure:"postgres"`
	Models      []string          `mapstructure:"models"`
	RealModels  RealModelsConfig  `mapstructure:"real_models"`
	CircuitBreaker CircuitBreakerConfig      `mapstructure:"circuit_breaker"`
	RateLimit      RateLimitConfig           `mapstructure:"rate_limit"`
	Providers      map[string]ProviderConfig `mapstructure:"providers"`
	Token          TokenConfig               `mapstructure:"token"`
	Metrics        MetricsConfig             `mapstructure:"metrics"`
	Health         HealthConfig              `mapstructure:"health"`
	APIKeys        []APIKeyConfig            `mapstructure:"api_keys"`
	AuthWhitelist  []string                  `mapstructure:"auth_whitelist"`
}

type AppConfig struct {
	Name    string `mapstructure:"name"`
	Version string `mapstructure:"version"`
	Env     string `mapstructure:"env"`
	Port    int    `mapstructure:"port"`
}

type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
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

type PostgresConfig struct {
	Host        string        `mapstructure:"host"`
	Port        int           `mapstructure:"port"`
	User        string        `mapstructure:"user"`
	Password    string        `mapstructure:"password"`
	Database    string        `mapstructure:"database"`
	DSN         string        `mapstructure:"dsn"`
	SSLMode     string        `mapstructure:"ssl_mode"`
	MaxConns    int32         `mapstructure:"max_conns"`
	MinConns    int32         `mapstructure:"min_conns"`
	ConnTimeout time.Duration `mapstructure:"conn_timeout"`
}

type RealModelsConfig struct {
	Strategy string         `mapstructure:"strategy"`
	Models   []FallbackItem `mapstructure:"models"`
}

type FallbackItem struct {
	Provider string        `mapstructure:"provider"`
	Model    string        `mapstructure:"model"`
	Weight   int           `mapstructure:"weight"`
	Timeout  time.Duration `mapstructure:"timeout"`
	Retry    int           `mapstructure:"retry"`
	Cost     float64       `mapstructure:"cost"` // 单位请求成本（元），用于 cost_optimized 策略
}

type CircuitBreakerConfig struct {
	MaxRequests      uint          `mapstructure:"max_requests"`
	Interval         time.Duration `mapstructure:"interval"`
	Timeout          time.Duration `mapstructure:"timeout"`
	FailureThreshold int           `mapstructure:"failure_threshold"`
	Cooldown         time.Duration `mapstructure:"cooldown"`
}

type RateLimitConfig struct {
	Enabled           bool `mapstructure:"enabled"`
	RequestsPerSecond int  `mapstructure:"requests_per_second"`
	Burst             int  `mapstructure:"burst"`
}

type ProviderConfig struct {
	BaseURL  string        `mapstructure:"base_url"`
	APIKey   string        `mapstructure:"api_key"`
	Timeout  time.Duration `mapstructure:"timeout"`
	Endpoint string        `mapstructure:"endpoint"` // 可选：覆盖默认的 upstream 端点路径（如 /messages, /chat/completions）
	Protocol string        `mapstructure:"protocol"` // 上游协议："openai"（默认）| "anthropic"
}

type TokenConfig struct {
	TokenizerMapping map[string]string `mapstructure:"tokenizer_mapping"`
}

type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
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
	v := viper.NewWithOptions(viper.KeyDelimiter("::"))
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	// 使用 :: 作为 key 分隔符，避免模型名中包含 "." 导致的解析问题
	// （如 mimo-v2.5 会被默认的 "." 分隔符拆成 mimo-v2 -> 5）

	// 环境变量支持
	v.SetEnvPrefix("LLM")
	v.SetEnvKeyReplacer(strings.NewReplacer("::", "_"))
	v.AutomaticEnv()

	// 默认值
	v.SetDefault("app::port", 8080)
	v.SetDefault("log::level", "info")
	v.SetDefault("redis::addr", "localhost:6379")
	v.SetDefault("postgres::host", "localhost")
	v.SetDefault("postgres::port", 5432)
	v.SetDefault("postgres::ssl_mode", "disable")
	v.SetDefault("postgres::max_conns", int32(20))
	v.SetDefault("postgres::min_conns", int32(0))
	v.SetDefault("postgres::conn_timeout", 5*time.Second)
	v.SetDefault("health::enabled", true)
	v.SetDefault("health::path", "/health")
	v.SetDefault("metrics::enabled", true)
	v.SetDefault("metrics::path", "/metrics")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config failed: %w", err)
	}

	// 加载环境特定配置（如 config.prod.yaml）
	env := v.GetString("app::env")
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
