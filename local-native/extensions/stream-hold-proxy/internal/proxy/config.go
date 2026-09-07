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
	listenAddr, err := requiredEnvString("STREAM_HOLD_LISTEN_ADDR")
	if err != nil {
		return Config{}, err
	}
	upstreamValue, err := requiredEnvString("STREAM_HOLD_UPSTREAM_URL")
	if err != nil {
		return Config{}, err
	}
	upstreamURL, err := url.Parse(upstreamValue)
	if err != nil {
		return Config{}, fmt.Errorf("parse STREAM_HOLD_UPSTREAM_URL: %w", err)
	}
	protectedPathsValue, err := requiredEnvString("STREAM_HOLD_PATHS")
	if err != nil {
		return Config{}, err
	}
	enabled, err := envBool("STREAM_HOLD_ENABLED")
	if err != nil {
		return Config{}, err
	}
	stateFile, err := requiredEnvString("STREAM_HOLD_STATE_FILE")
	if err != nil {
		return Config{}, err
	}
	spoolDir, err := requiredEnvString("STREAM_HOLD_SPOOL_DIR")
	if err != nil {
		return Config{}, err
	}
	keepaliveInterval, err := envDuration("STREAM_HOLD_KEEPALIVE_INTERVAL")
	if err != nil {
		return Config{}, err
	}
	retryMinInterval, err := envDuration("STREAM_HOLD_RETRY_MIN_INTERVAL")
	if err != nil {
		return Config{}, err
	}
	retryMaxInterval, err := envDuration("STREAM_HOLD_RETRY_MAX_INTERVAL")
	if err != nil {
		return Config{}, err
	}
	retryJitterRatio, err := envFloat("STREAM_HOLD_RETRY_JITTER_RATIO")
	if err != nil {
		return Config{}, err
	}
	responseHeaderTimeout, err := envDuration("STREAM_HOLD_RESPONSE_HEADER_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	streamIdleTimeout, err := envDuration("STREAM_HOLD_STREAM_IDLE_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	attemptMaxDuration, err := envDuration("STREAM_HOLD_ATTEMPT_MAX_DURATION")
	if err != nil {
		return Config{}, err
	}
	maxRequestBodyBytes, err := envInt64("STREAM_HOLD_MAX_REQUEST_BODY_BYTES")
	if err != nil {
		return Config{}, err
	}
	maxAttemptBodyBytes, err := envInt64("STREAM_HOLD_MAX_ATTEMPT_BODY_BYTES")
	if err != nil {
		return Config{}, err
	}
	maxSSELineBytes, err := envInt("STREAM_HOLD_MAX_SSE_LINE_BYTES")
	if err != nil {
		return Config{}, err
	}
	maxIdleConnsPerHost, err := envInt("STREAM_HOLD_MAX_IDLE_CONNS_PER_HOST")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		ListenAddr:            listenAddr,
		UpstreamURL:           upstreamURL,
		ProtectedPaths:        splitPaths(protectedPathsValue),
		Enabled:               enabled,
		StateFile:             stateFile,
		SpoolDir:              spoolDir,
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
		if _, supported := streamProtocolForPath(path); !supported {
			return fmt.Errorf("protected path %q has no stream protocol contract", path)
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

func (c Config) protocolForPath(path string) (streamProtocol, bool) {
	for _, protectedPath := range c.ProtectedPaths {
		if path == protectedPath {
			return streamProtocolForPath(path)
		}
	}
	return "", false
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

func requiredEnvString(key string) (string, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return strings.TrimSpace(value), nil
}

func envBool(key string) (bool, error) {
	value, err := requiredEnvString(key)
	if err != nil {
		return false, err
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func envDuration(key string) (time.Duration, error) {
	value, err := requiredEnvString(key)
	if err != nil {
		return 0, err
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return parsed, nil
}

func envFloat(key string) (float64, error) {
	value, err := requiredEnvString(key)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", key, err)
	}
	return parsed, nil
}

func envInt64(key string) (int64, error) {
	value, err := requiredEnvString(key)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func envInt(key string) (int, error) {
	value, err := envInt64(key)
	if err != nil {
		return 0, err
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("%s is outside the supported integer range", key)
	}
	return int(value), nil
}
