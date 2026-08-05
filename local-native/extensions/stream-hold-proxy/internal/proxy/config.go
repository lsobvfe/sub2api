package proxy

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const controlPrefix = "/_stream-hold"

type Config struct {
	ListenAddr            string
	UpstreamURL           *url.URL
	ProtectedPaths        []string
	Enabled               bool
	StateFile             string
	SpoolDir              string
	KeepaliveInterval     time.Duration
	RetryMinInterval      time.Duration
	RetryMaxInterval      time.Duration
	RetryJitterRatio      float64
	ResponseHeaderTimeout time.Duration
	StreamIdleTimeout     time.Duration
	AttemptMaxDuration    time.Duration
	MaxRequestBodyBytes   int64
	MaxAttemptBodyBytes   int64
	MaxSSELineBytes       int
	MaxIdleConnsPerHost   int
}

func LoadConfig() (Config, error) {
	upstreamURL, err := url.Parse(envString("STREAM_HOLD_UPSTREAM_URL", "http://127.0.0.1:18082"))
	if err != nil {
		return Config{}, fmt.Errorf("parse STREAM_HOLD_UPSTREAM_URL: %w", err)
	}
	enabled, err := envBool("STREAM_HOLD_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	keepaliveInterval, err := envDuration("STREAM_HOLD_KEEPALIVE_INTERVAL", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	retryMinInterval, err := envDuration("STREAM_HOLD_RETRY_MIN_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	retryMaxInterval, err := envDuration("STREAM_HOLD_RETRY_MAX_INTERVAL", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	retryJitterRatio, err := envFloat("STREAM_HOLD_RETRY_JITTER_RATIO", 0.2)
	if err != nil {
		return Config{}, err
	}
	responseHeaderTimeout, err := envDuration("STREAM_HOLD_RESPONSE_HEADER_TIMEOUT", 2*time.Minute)
	if err != nil {
		return Config{}, err
	}
	streamIdleTimeout, err := envDuration("STREAM_HOLD_STREAM_IDLE_TIMEOUT", 90*time.Second)
	if err != nil {
		return Config{}, err
	}
	attemptMaxDuration, err := envDuration("STREAM_HOLD_ATTEMPT_MAX_DURATION", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	maxRequestBodyBytes, err := envInt64("STREAM_HOLD_MAX_REQUEST_BODY_BYTES", 64*1024*1024)
	if err != nil {
		return Config{}, err
	}
	maxAttemptBodyBytes, err := envInt64("STREAM_HOLD_MAX_ATTEMPT_BODY_BYTES", 512*1024*1024)
	if err != nil {
		return Config{}, err
	}
	maxSSELineBytes, err := envInt("STREAM_HOLD_MAX_SSE_LINE_BYTES", 16*1024*1024)
	if err != nil {
		return Config{}, err
	}
	maxIdleConnsPerHost, err := envInt("STREAM_HOLD_MAX_IDLE_CONNS_PER_HOST", 256)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		ListenAddr:            envString("STREAM_HOLD_LISTEN_ADDR", "0.0.0.0:18081"),
		UpstreamURL:           upstreamURL,
		ProtectedPaths:        splitPaths(envString("STREAM_HOLD_PATHS", "/responses,/v1/responses")),
		Enabled:               enabled,
		StateFile:             envString("STREAM_HOLD_STATE_FILE", "./stream-hold-proxy-state.json"),
		SpoolDir:              envString("STREAM_HOLD_SPOOL_DIR", "./stream-hold-spool"),
		KeepaliveInterval:     keepaliveInterval,
		RetryMinInterval:      retryMinInterval,
		RetryMaxInterval:      retryMaxInterval,
		RetryJitterRatio:      retryJitterRatio,
		ResponseHeaderTimeout: responseHeaderTimeout,
		StreamIdleTimeout:     streamIdleTimeout,
		AttemptMaxDuration:    attemptMaxDuration,
		MaxRequestBodyBytes:   maxRequestBodyBytes,
		MaxAttemptBodyBytes:   maxAttemptBodyBytes,
		MaxSSELineBytes:       maxSSELineBytes,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return fmt.Errorf("STREAM_HOLD_LISTEN_ADDR is required")
	}
	if c.UpstreamURL == nil || (c.UpstreamURL.Scheme != "http" && c.UpstreamURL.Scheme != "https") || c.UpstreamURL.Host == "" {
		return fmt.Errorf("STREAM_HOLD_UPSTREAM_URL must be an absolute http or https URL")
	}
	if len(c.ProtectedPaths) == 0 {
		return fmt.Errorf("STREAM_HOLD_PATHS must contain at least one path")
	}
	for _, path := range c.ProtectedPaths {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("protected path %q must start with /", path)
		}
		if strings.HasPrefix(path, controlPrefix) {
			return fmt.Errorf("protected path %q conflicts with the control endpoint", path)
		}
	}
	if c.KeepaliveInterval <= 0 {
		return fmt.Errorf("STREAM_HOLD_KEEPALIVE_INTERVAL must be positive")
	}
	if c.RetryMinInterval < 0 || c.RetryMaxInterval < c.RetryMinInterval {
		return fmt.Errorf("retry intervals are invalid")
	}
	if c.RetryJitterRatio < 0 || c.RetryJitterRatio > 1 {
		return fmt.Errorf("STREAM_HOLD_RETRY_JITTER_RATIO must be within [0,1]")
	}
	if c.ResponseHeaderTimeout <= 0 || c.StreamIdleTimeout <= 0 || c.AttemptMaxDuration <= 0 {
		return fmt.Errorf("upstream timeouts must be positive")
	}
	if c.MaxRequestBodyBytes <= 0 || c.MaxAttemptBodyBytes <= 0 || c.MaxSSELineBytes <= 0 {
		return fmt.Errorf("body and SSE limits must be positive")
	}
	if c.MaxIdleConnsPerHost <= 0 {
		return fmt.Errorf("STREAM_HOLD_MAX_IDLE_CONNS_PER_HOST must be positive")
	}
	if strings.TrimSpace(c.StateFile) == "" || strings.TrimSpace(c.SpoolDir) == "" {
		return fmt.Errorf("state file and spool directory are required")
	}
	return nil
}

func (c Config) protects(path string) bool {
	for _, protectedPath := range c.ProtectedPaths {
		if path == protectedPath {
			return true
		}
	}
	return false
}

func splitPaths(value string) []string {
	seen := make(map[string]struct{})
	paths := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		path := strings.TrimSpace(item)
		if path == "" {
			continue
		}
		if len(path) > 1 {
			path = strings.TrimRight(path, "/")
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return parsed, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", key, err)
	}
	return parsed, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func envInt(key string, fallback int) (int, error) {
	value, err := envInt64(key, int64(fallback))
	if err != nil {
		return 0, err
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("%s is outside the supported integer range", key)
	}
	return int(value), nil
}
