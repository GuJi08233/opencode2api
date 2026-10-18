// Package config loads, validates, and redacts service configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"opencode2api/internal/jsonutil"
	wire "opencode2api/internal/protocol"
)

type Config struct {
	Listen            string                       `json:"listen"`
	ServerKeys        []string                     `json:"server_keys"`
	ServerKeyMetadata map[string]ServerKeyMetadata `json:"server_key_metadata,omitempty"`
	Proxies           []string                     `json:"proxies"`
	ProxyFile         string                       `json:"proxyfile"`
	Upstream          UpstreamConfig               `json:"upstream"`
	Retry             RetryConfig                  `json:"retry"`
	Models            ModelsConfig                 `json:"models"`
	Performance       PerformanceConfig            `json:"performance"`
	Logging           LoggingConfig                `json:"logging"`
	Reasoning         ReasoningConfig              `json:"reasoning"`

	effectiveProxies []string
	enabledKeySet    map[string]string
}

type ReasoningConfig struct {
	Effort        string            `json:"effort,omitempty"`
	EffortByModel map[string]string `json:"effort_by_model,omitempty"`
}

type UpstreamConfig struct {
	// Zen is the legacy /zen/v1 free-tier origin (default https://opencode.ai/zen),
	// tried first before falling back to Anonymous.
	Zen string `json:"zen"`
	// Anonymous is the /inference free-tier origin (default https://opencode.ai/inference).
	Anonymous string `json:"anonymous"`
}

type RetryConfig struct {
	MaxAttempts          int `json:"max_attempts"`
	AnonymousMaxAttempts int `json:"anonymous_max_attempts"`
	TimeoutSeconds       int `json:"timeout_seconds"`
}

type ModelsConfig struct {
	RefreshSeconds int               `json:"refresh_seconds"`
	Protocols      map[string]string `json:"protocols"`
}

type LoggingConfig struct {
	Level             string `json:"level"`
	DumpRequestBodies bool   `json:"dump_request_bodies"`
}

type PerformanceConfig struct {
	DisableProxyKeepAlives   bool `json:"disable_proxy_keep_alives"`
	MaxIdleConns             int  `json:"max_idle_conns"`
	MaxIdleConnsPerHost      int  `json:"max_idle_conns_per_host"`
	MaxConnsPerHost          int  `json:"max_conns_per_host"`
	IdleConnTimeoutSeconds   int  `json:"idle_conn_timeout_seconds"`
	ConnectTimeoutSeconds    int  `json:"connect_timeout_seconds"`
	FailureCooldownSeconds   int  `json:"failure_cooldown_seconds"`
	AttemptTimeoutSeconds    int  `json:"attempt_timeout_seconds"`
	FirstEventTimeoutSeconds int  `json:"first_event_timeout_seconds"`
}

func (cfg PerformanceConfig) AttemptTimeout(requestTimeout time.Duration) time.Duration {
	if cfg.AttemptTimeoutSeconds > 0 {
		attempt := time.Duration(cfg.AttemptTimeoutSeconds) * time.Second
		if requestTimeout > 0 && attempt > requestTimeout {
			return requestTimeout
		}
		return attempt
	}
	return requestTimeout
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	data, err = stripJSONComments(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg := Config{
		Listen:      "127.0.0.1:8080",
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Anonymous: "https://opencode.ai/inference"},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 4096, MaxIdleConnsPerHost: 1024, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 120, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:     LoggingConfig{Level: "info"},
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := jsonutil.EnsureEOF(dec); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return Normalize(path, cfg)
}

func Normalize(path string, cfg Config) (Config, error) {
	trimList(&cfg.ServerKeys)
	cfg.ProxyFile = strings.TrimSpace(cfg.ProxyFile)
	if err := resolveProxyFiles(path, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Listen == "" {
		return Config{}, errors.New("listen must not be empty")
	}
	cfg.Upstream.Zen = strings.TrimSpace(cfg.Upstream.Zen)
	if cfg.Upstream.Zen == "" {
		cfg.Upstream.Zen = "https://opencode.ai/zen"
	}
	cfg.Upstream.Anonymous = strings.TrimSpace(cfg.Upstream.Anonymous)
	if cfg.Upstream.Anonymous == "" {
		cfg.Upstream.Anonymous = "https://opencode.ai/inference"
	}
	for name, raw := range map[string]string{"upstream.zen": cfg.Upstream.Zen, "upstream.anonymous": cfg.Upstream.Anonymous} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") {
			return Config{}, fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	if len(cfg.ServerKeys) == 0 {
		return Config{}, errors.New("server_keys must contain at least one local key")
	}
	cfg.enabledKeySet = make(map[string]string, len(cfg.ServerKeys))
	for _, key := range cfg.ServerKeys {
		if cfg.ServerKeyEnabled(key) {
			cfg.enabledKeySet[key] = Fingerprint(key)
		}
	}
	if len(cfg.enabledKeySet) == 0 {
		return Config{}, errors.New("at least one server_keys entry must be enabled")
	}
	if cfg.Retry.MaxAttempts < 1 {
		return Config{}, errors.New("retry.max_attempts must be at least 1")
	}
	if cfg.Retry.AnonymousMaxAttempts < 0 {
		return Config{}, errors.New("retry.anonymous_max_attempts must not be negative")
	}
	if cfg.Retry.AnonymousMaxAttempts > 0 && !cfg.Performance.DisableProxyKeepAlives {
		return Config{}, errors.New("retry.anonymous_max_attempts requires performance.disable_proxy_keep_alives for rotating proxy gateways")
	}
	if cfg.Retry.TimeoutSeconds < 1 {
		return Config{}, errors.New("retry.timeout_seconds must be at least 1")
	}
	if cfg.Models.RefreshSeconds < 1 {
		return Config{}, errors.New("models.refresh_seconds must be at least 1")
	}
	if cfg.Performance.MaxIdleConns < 1 || cfg.Performance.MaxIdleConnsPerHost < 1 || cfg.Performance.MaxConnsPerHost < 0 || cfg.Performance.IdleConnTimeoutSeconds < 1 || cfg.Performance.ConnectTimeoutSeconds < 1 || cfg.Performance.FailureCooldownSeconds < 1 {
		return Config{}, errors.New("performance values must be positive (max_conns_per_host may be zero for unlimited)")
	}
	if cfg.Performance.AttemptTimeoutSeconds < 0 {
		return Config{}, errors.New("performance.attempt_timeout_seconds must not be negative (0 keeps the retry timeout)")
	}
	if cfg.Performance.FirstEventTimeoutSeconds < 0 || cfg.Performance.FirstEventTimeoutSeconds > 86400 {
		return Config{}, errors.New("performance.first_event_timeout_seconds must be between 0 and 86400 (0 disables the first-event timeout)")
	}
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		return Config{}, errors.New("logging.level must be debug, info, warn, or error")
	}
	for _, raw := range cfg.RuntimeProxies() {
		if raw == "direct" {
			continue
		}
		clean, _ := ParseProxyEntry(raw)
		pu, err := url.Parse(clean)
		if err != nil || pu.Host == "" {
			return Config{}, fmt.Errorf("invalid proxy URL %q", RedactURL(raw))
		}
		switch strings.ToLower(pu.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("unsupported proxy scheme %q", pu.Scheme)
		}
	}
	for model, protocol := range cfg.Models.Protocols {
		if model == "" || !wire.Valid(wire.Protocol(protocol)) {
			return Config{}, fmt.Errorf("models.protocols contains invalid mapping %q: %q", model, protocol)
		}
	}
	if err := normalizeReasoning(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeReasoning(cfg *Config) error {
	cfg.Reasoning.Effort = strings.ToLower(strings.TrimSpace(cfg.Reasoning.Effort))
	if err := validateEffort("reasoning.effort", cfg.Reasoning.Effort); err != nil {
		return err
	}
	if len(cfg.Reasoning.EffortByModel) == 0 {
		cfg.Reasoning.EffortByModel = nil
		return nil
	}
	normalized := make(map[string]string, len(cfg.Reasoning.EffortByModel))
	for model, effort := range cfg.Reasoning.EffortByModel {
		key := strings.TrimSpace(model)
		if key == "" {
			return errors.New("reasoning.effort_by_model must not contain an empty model ID")
		}
		level := strings.ToLower(strings.TrimSpace(effort))
		if err := validateEffort(fmt.Sprintf("reasoning.effort_by_model[%q]", key), level); err != nil {
			return err
		}
		normalized[key] = level
	}
	cfg.Reasoning.EffortByModel = normalized
	return nil
}

func validateEffort(name, value string) error {
	switch value {
	case "", "auto", "minimal", "low", "medium", "high", "xhigh", "max", "none":
		return nil
	default:
		return fmt.Errorf("%s must be one of auto, minimal, low, medium, high, xhigh, max, none, or empty to disable the override", name)
	}
}

func (cfg Config) ForcedEffort(model string) string {
	if effort, ok := cfg.Reasoning.EffortByModel[model]; ok {
		return effort
	}
	return cfg.Reasoning.Effort
}

// AuthenticateKey checks whether candidate is an enabled server key in O(1)
// average time and returns its fingerprint if valid.
func (cfg Config) AuthenticateKey(candidate string) (string, bool) {
	if candidate == "" {
		return "", false
	}
	if fp, ok := cfg.enabledKeySet[candidate]; ok {
		return fp, true
	}
	return "", false
}

func (cfg Config) RuntimeProxies() []string {
	if len(cfg.effectiveProxies) > 0 {
		return cfg.effectiveProxies
	}
	if len(cfg.Proxies) > 0 {
		return cfg.Proxies
	}
	return []string{"direct"}
}
