package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config 全局配置
type Config struct {
	App            AppConfig                 `mapstructure:"app" yaml:"app"`
	Log            LogConfig                 `mapstructure:"log" yaml:"log"`
	Redis          RedisConfig               `mapstructure:"redis" yaml:"redis"`
	Postgres       PostgresConfig            `mapstructure:"postgres" yaml:"postgres"`
	Models         []ModelEntry              `mapstructure:"models" yaml:"models"`
	RealModels     RealModelsConfig          `mapstructure:"real_models" yaml:"real_models"`
	CircuitBreaker CircuitBreakerConfig      `mapstructure:"circuit_breaker" yaml:"circuit_breaker"`
	RateLimit      RateLimitConfig           `mapstructure:"rate_limit" yaml:"rate_limit"`
	Providers      map[string]ProviderConfig `mapstructure:"providers" yaml:"providers"`
	Token          TokenConfig               `mapstructure:"token" yaml:"token"`
	Health         HealthConfig              `mapstructure:"health" yaml:"health"`
	Admin          AdminConfig               `mapstructure:"admin" yaml:"admin"`
	APIKeys        []APIKeyConfig            `mapstructure:"api_keys" yaml:"api_keys"`
	AuthWhitelist  []string                  `mapstructure:"auth_whitelist" yaml:"auth_whitelist"`

	filePath string `mapstructure:"-" yaml:"-"` // 配置文件路径
}

type AppConfig struct {
	Name    string `mapstructure:"name" yaml:"name"`
	Version string `mapstructure:"version" yaml:"version"`
	Env     string `mapstructure:"env" yaml:"env"`
	Port    int    `mapstructure:"port" yaml:"port"`
}

type LogConfig struct {
	Level  string `mapstructure:"level" yaml:"level"`
	Format string `mapstructure:"format" yaml:"format"`
	Output string `mapstructure:"output" yaml:"output"`
}

type RedisConfig struct {
	Addr         string        `mapstructure:"addr" yaml:"addr"`
	Password     string        `mapstructure:"password" yaml:"password"`
	DB           int           `mapstructure:"db" yaml:"db"`
	PoolSize     int           `mapstructure:"pool_size" yaml:"pool_size"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout" yaml:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout" yaml:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout" yaml:"write_timeout"`
}

type PostgresConfig struct {
	Host        string        `mapstructure:"host" yaml:"host"`
	Port        int           `mapstructure:"port" yaml:"port"`
	User        string        `mapstructure:"user" yaml:"user"`
	Password    string        `mapstructure:"password" yaml:"password"`
	Database    string        `mapstructure:"database" yaml:"database"`
	DSN         string        `mapstructure:"dsn" yaml:"dsn"`
	SSLMode     string        `mapstructure:"ssl_mode" yaml:"ssl_mode"`
	MaxConns    int32         `mapstructure:"max_conns" yaml:"max_conns"`
	MinConns    int32         `mapstructure:"min_conns" yaml:"min_conns"`
	ConnTimeout time.Duration `mapstructure:"conn_timeout" yaml:"conn_timeout"`
}

type ModelEntry struct {
	Name string `json:"id" mapstructure:"name" yaml:"name"`
	Tier string `json:"tier" mapstructure:"tier" yaml:"tier"`
}

type RealModelsConfig struct {
	Strategy string         `mapstructure:"strategy" yaml:"strategy"`
	Models   []FallbackItem `mapstructure:"models" yaml:"models"`
}

type FallbackItem struct {
	Provider string        `json:"provider" mapstructure:"provider" yaml:"provider"`
	Model    string        `json:"model" mapstructure:"model" yaml:"model"`
	Weight   int           `json:"weight" mapstructure:"weight" yaml:"weight"`
	Timeout  time.Duration `json:"timeout" mapstructure:"timeout" yaml:"timeout"`
	Retry    int           `json:"retry" mapstructure:"retry" yaml:"retry"`
	Cost     float64       `json:"cost" mapstructure:"cost" yaml:"cost"`
	Tier     string        `json:"tier" mapstructure:"tier" yaml:"tier"`
}

type CircuitBreakerConfig struct {
	MaxRequests      uint          `mapstructure:"max_requests" yaml:"max_requests"`
	Interval         time.Duration `mapstructure:"interval" yaml:"interval"`
	Timeout          time.Duration `mapstructure:"timeout" yaml:"timeout"`
	FailureThreshold int           `mapstructure:"failure_threshold" yaml:"failure_threshold"`
	Cooldown         time.Duration `mapstructure:"cooldown" yaml:"cooldown"`
}

type RateLimitConfig struct {
	Enabled           bool `mapstructure:"enabled" yaml:"enabled"`
	RequestsPerSecond int  `mapstructure:"requests_per_second" yaml:"requests_per_second"`
	Burst             int  `mapstructure:"burst" yaml:"burst"`
}

type ProviderConfig struct {
	BaseURL  string        `mapstructure:"base_url" yaml:"base_url"`
	APIKey   string        `mapstructure:"api_key" yaml:"api_key"`
	Timeout  time.Duration `mapstructure:"timeout" yaml:"timeout"`
	Endpoint string        `mapstructure:"endpoint" yaml:"endpoint"`
	Protocol string        `mapstructure:"protocol" yaml:"protocol"`
}

type TokenConfig struct {
	TokenizerMapping map[string]string `mapstructure:"tokenizer_mapping" yaml:"tokenizer_mapping"`
}

type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled" yaml:"enabled"`
	Path    string `mapstructure:"path" yaml:"path"`
}

type HealthConfig struct {
	Enabled bool   `mapstructure:"enabled" yaml:"enabled"`
	Path    string `mapstructure:"path" yaml:"path"`
}

type AdminConfig struct {
	Password    string        `mapstructure:"password" yaml:"password"`
	JWTSecret   string        `mapstructure:"jwt_secret" yaml:"jwt_secret"`
	TokenExpiry time.Duration `mapstructure:"token_expiry" yaml:"token_expiry"`
}

type APIKeyConfig struct {
	Key  string `mapstructure:"key" yaml:"key"`
	Name string `mapstructure:"name" yaml:"name"`
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
	v.SetDefault("admin::password", "")

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

	// 解析环境变量中的敏感字段（如 ${ENV_VAR}）
	for name, p := range cfg.Providers {
		p.APIKey = resolveEnv(p.APIKey)
		cfg.Providers[name] = p
	}
	cfg.Postgres.Password = resolveEnv(cfg.Postgres.Password)
	cfg.Redis.Password = resolveEnv(cfg.Redis.Password)
	// 解析 admin 密码和 JWT Secret（支持 ${ENV_VAR}，未设置则使用空字符串）
	cfg.Admin.Password = resolveEnv(cfg.Admin.Password)
	cfg.Admin.JWTSecret = resolveEnv(cfg.Admin.JWTSecret)
	if strings.HasPrefix(cfg.Admin.Password, "${") {
		cfg.Admin.Password = ""
	}
	if strings.HasPrefix(cfg.Admin.JWTSecret, "${") {
		cfg.Admin.JWTSecret = ""
	}
	cfg.filePath = path

	// 如果 JWT Secret 为空，自动生成
	if cfg.Admin.JWTSecret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err == nil {
			cfg.Admin.JWTSecret = hex.EncodeToString(buf)
		}
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

// Save 将配置持久化到 YAML 文件
func (c *Config) Save() error {
	if c.filePath == "" {
		return fmt.Errorf("config file path not set")
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config failed: %w", err)
	}
	return os.WriteFile(c.filePath, data, 0644)
}
