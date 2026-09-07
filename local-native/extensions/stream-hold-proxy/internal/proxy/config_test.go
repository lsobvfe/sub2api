package proxy

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConfigRejectsPathWithoutProtocolContract(t *testing.T) {
	upstreamURL, err := url.Parse("http://127.0.0.1:18082")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ListenAddr:            "127.0.0.1:18081",
		UpstreamURL:           upstreamURL,
		ProtectedPaths:        []string{"/v1/unknown-stream"},
		Enabled:               true,
		StateFile:             "state.json",
		SpoolDir:              "spool",
		KeepaliveInterval:     time.Second,
		RetryMinInterval:      time.Second,
		RetryMaxInterval:      time.Second,
		ResponseHeaderTimeout: time.Second,
		StreamIdleTimeout:     time.Second,
		AttemptMaxDuration:    time.Second,
		MaxRequestBodyBytes:   1,
		MaxAttemptBodyBytes:   1,
		MaxSSELineBytes:       1,
		MaxIdleConnsPerHost:   1,
	}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate() succeeded for an unknown stream protocol path")
	}
	if !strings.Contains(err.Error(), "has no stream protocol contract") {
		t.Fatalf("Validate() error = %v", err)
	}
}
