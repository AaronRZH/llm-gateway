package config

import (
	"bytes"
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
	Name         string        `mapstructure:"name" yaml:"name"`
	Version      string        `mapstructure:"version" yaml:"version"`
	Env          string        `mapstructure:"env" yaml:"env"`
	Port         int           `mapstructure:"port" yaml:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout" yaml:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout" yaml:"write_timeout"`
	IdleTimeout  time.Duration `mapstructure:"idle_timeout" yaml:"idle_timeout"`
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
	Disabled bool          `json:"disabled" mapstructure:"disabled" yaml:"disabled"`
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
	Protocol string        `mapstructure:"protocol" yaml:"protocol"`
}

type TokenConfig struct {
	TokenizerMapping map[string]string `mapstructure:"tokenizer_mapping" yaml:"tokenizer_mapping"`
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
	v.SetDefault("app::read_timeout", 60*time.Second)
	v.SetDefault("app::write_timeout", 120*time.Second)
	v.SetDefault("app::idle_timeout", 120*time.Second)
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

// Save 将配置持久化到 YAML 文件（保留注释和 ${ENV_VAR} 引用）
// 注意：此方法重新序列化整个配置，会丢失 YAML 注释。
// 推荐使用 SaveProvider / AppendAPIKey 等手术式方法。
func (c *Config) Save() error {
	if c.filePath == "" {
		return fmt.Errorf("config file path not set")
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(c); err != nil {
		return fmt.Errorf("encode config failed: %w", err)
	}
	encoder.Close()
	return os.WriteFile(c.filePath, buf.Bytes(), 0644)
}

// ==================== 手术式 YAML 持久化 ====================
// 以下方法读取原始 YAML → 修改指定节点 → 写回，保留注释、格式和 ${ENV_VAR} 引用。

// readYAMLDoc 读取 YAML 文件并解析为 Node 树
func (c *Config) readYAMLDoc() (*yaml.Node, error) {
	data, err := os.ReadFile(c.filePath)
	if err != nil {
		return nil, fmt.Errorf("read config failed: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config failed: %w", err)
	}
	return &doc, nil
}

// writeYAMLDoc 将 Node 树写回 YAML 文件（统一用 2 空格缩进）
func (c *Config) writeYAMLDoc(doc *yaml.Node) error {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return fmt.Errorf("encode config failed: %w", err)
	}
	encoder.Close()
	return os.WriteFile(c.filePath, buf.Bytes(), 0644)
}

// findMappingKey 在 MappingNode 中查找指定 key 的 value node
// MappingNode 的 Content 是交替的 [key1, value1, key2, value2, ...]
func findMappingKey(node *yaml.Node, key string) (int, *yaml.Node) {
	if node.Kind != yaml.MappingNode {
		return -1, nil
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			return i + 1, node.Content[i+1]
		}
	}
	return -1, nil
}

// findMappingIndex 在 MappingNode 中查找指定 key 的索引位置
func findMappingIndex(node *yaml.Node, key string) int {
	if node.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// valueToNode 将任意值序列化为 yaml.Node
func valueToNode(v interface{}) *yaml.Node {
	data, err := yaml.Marshal(v)
	if err != nil {
		// 回退：标量字符串
		return &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%v", v), Tag: "!!str"}
	}
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%v", v), Tag: "!!str"}
	}
	// Document 包装，取第一个子节点
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return &n
}

// setMappingKey 在 MappingNode 中设置 key→value（key 存在则替换，不存在则追加）
func setMappingKey(node *yaml.Node, key string, value interface{}) {
	if node.Kind != yaml.MappingNode {
		return
	}
	idx := findMappingIndex(node, key)
	valNode := valueToNode(value)
	if idx >= 0 {
		// 替换已有 key 对应的 value node
		node.Content[idx+1] = valNode
	} else {
		// 追加新 key-value
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}, valNode)
	}
}

// deleteMappingKey 在 MappingNode 中删除指定 key 及其 value
func deleteMappingKey(node *yaml.Node, key string) {
	if node.Kind != yaml.MappingNode {
		return
	}
	idx := findMappingIndex(node, key)
	if idx < 0 {
		return
	}
	node.Content = append(node.Content[:idx], node.Content[idx+2:]...)
}

// ==================== 手术式方法 ====================

// SaveProvider 新增或更新 Provider 配置（保留注释和格式）
func (c *Config) SaveProvider(name string, p ProviderConfig) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0] // Document → Mapping
	_, providersNode := findMappingKey(root, "providers")
	if providersNode == nil {
		return fmt.Errorf("providers key not found in config")
	}
	setMappingKey(providersNode, name, p)
	return c.writeYAMLDoc(doc)
}

// DeleteProvider 删除 Provider 配置
func (c *Config) DeleteProvider(name string) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, providersNode := findMappingKey(root, "providers")
	if providersNode == nil {
		return fmt.Errorf("providers key not found in config")
	}
	deleteMappingKey(providersNode, name)
	return c.writeYAMLDoc(doc)
}

// AppendAPIKey 追加一个 API Key 到 api_keys 列表
func (c *Config) AppendAPIKey(k APIKeyConfig) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, keysNode := findMappingKey(root, "api_keys")
	if keysNode != nil && keysNode.Kind == yaml.SequenceNode {
		keysNode.Content = append(keysNode.Content, valueToNode(k))
		return c.writeYAMLDoc(doc)
	}
	return fmt.Errorf("api_keys sequence not found in config")
}

// RemoveAPIKey 从 api_keys 列表中删除指定 key 的条目
func (c *Config) RemoveAPIKey(key string) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, keysNode := findMappingKey(root, "api_keys")
	if keysNode == nil || keysNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("api_keys sequence not found in config")
	}
	filtered := make([]*yaml.Node, 0, len(keysNode.Content))
	for _, item := range keysNode.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		_, keyNode := findMappingKey(item, "key")
		if keyNode != nil && keyNode.Value == key {
			continue // 跳过匹配项
		}
		filtered = append(filtered, item)
	}
	keysNode.Content = filtered
	return c.writeYAMLDoc(doc)
}

// AppendModel 追加一个虚拟模型
func (c *Config) AppendModel(m ModelEntry) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, modelsNode := findMappingKey(root, "models")
	if modelsNode != nil && modelsNode.Kind == yaml.SequenceNode {
		modelsNode.Content = append(modelsNode.Content, valueToNode(m))
		return c.writeYAMLDoc(doc)
	}
	return fmt.Errorf("models sequence not found in config")
}

// RemoveModel 从 models 列表中删除指定名称的虚拟模型
func (c *Config) RemoveModel(name string) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, modelsNode := findMappingKey(root, "models")
	if modelsNode == nil || modelsNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("models sequence not found in config")
	}
	filtered := make([]*yaml.Node, 0, len(modelsNode.Content))
	for _, item := range modelsNode.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		_, nameNode := findMappingKey(item, "name")
		if nameNode != nil && nameNode.Value == name {
			continue
		}
		filtered = append(filtered, item)
	}
	modelsNode.Content = filtered
	return c.writeYAMLDoc(doc)
}

// SaveStrategy 更新 real_models 的路由策略
func (c *Config) SaveStrategy(strategy string) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, realModelsNode := findMappingKey(root, "real_models")
	if realModelsNode == nil {
		return fmt.Errorf("real_models key not found in config")
	}
	setMappingKey(realModelsNode, "strategy", strategy)
	return c.writeYAMLDoc(doc)
}

// AppendRealModel 追加一个 real_model 条目
func (c *Config) AppendRealModel(m FallbackItem) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, realModelsNode := findMappingKey(root, "real_models")
	if realModelsNode == nil {
		return fmt.Errorf("real_models key not found in config")
	}
	_, modelsNode := findMappingKey(realModelsNode, "models")
	if modelsNode != nil && modelsNode.Kind == yaml.SequenceNode {
		modelsNode.Content = append(modelsNode.Content, valueToNode(m))
		return c.writeYAMLDoc(doc)
	}
	return fmt.Errorf("real_models.models sequence not found in config")
}

// UpdateRealModel 更新指定索引的 real_model 条目
func (c *Config) UpdateRealModel(index int, m FallbackItem) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, realModelsNode := findMappingKey(root, "real_models")
	if realModelsNode == nil {
		return fmt.Errorf("real_models key not found in config")
	}
	_, modelsNode := findMappingKey(realModelsNode, "models")
	if modelsNode == nil || modelsNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("real_models.models sequence not found in config")
	}
	if index < 0 || index >= len(modelsNode.Content) {
		return fmt.Errorf("index %d out of range", index)
	}
	modelsNode.Content[index] = valueToNode(m)
	return c.writeYAMLDoc(doc)
}

// RemoveRealModel 删除指定索引的 real_model 条目
func (c *Config) RemoveRealModel(index int) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, realModelsNode := findMappingKey(root, "real_models")
	if realModelsNode == nil {
		return fmt.Errorf("real_models key not found in config")
	}
	_, modelsNode := findMappingKey(realModelsNode, "models")
	if modelsNode == nil || modelsNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("real_models.models sequence not found in config")
	}
	if index < 0 || index >= len(modelsNode.Content) {
		return fmt.Errorf("index %d out of range", index)
	}
	modelsNode.Content = append(modelsNode.Content[:index], modelsNode.Content[index+1:]...)
	return c.writeYAMLDoc(doc)
}

// RemoveRealModelsByProvider 从 real_models.models 列表中删除指定 provider 的所有条目
func (c *Config) RemoveRealModelsByProvider(providerName string) error {
	doc, err := c.readYAMLDoc()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	_, realModelsNode := findMappingKey(root, "real_models")
	if realModelsNode == nil {
		return fmt.Errorf("real_models key not found in config")
	}
	_, modelsNode := findMappingKey(realModelsNode, "models")
	if modelsNode == nil || modelsNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("real_models.models sequence not found in config")
	}

	filtered := make([]*yaml.Node, 0, len(modelsNode.Content))
	for _, item := range modelsNode.Content {
		if item.Kind != yaml.MappingNode {
			filtered = append(filtered, item)
			continue
		}
		_, providerNode := findMappingKey(item, "provider")
		if providerNode == nil || providerNode.Value != providerName {
			filtered = append(filtered, item)
		}
	}
	modelsNode.Content = filtered
	return c.writeYAMLDoc(doc)
}
