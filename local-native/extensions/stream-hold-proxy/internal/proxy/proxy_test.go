package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyRetriesHTTPErrorWithoutExposingIt(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "Selected model is at capacity. Please try a different model.", http.StatusServiceUnavailable)
			return
		}
		writeCompletedStream(w, "ok")
	}))
	defer upstream.Close()

	body, status := makeHeldRequest(t, newTestProxy(t, upstream.URL, true))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if strings.Contains(body, "capacity") || strings.Contains(body, "503") {
		t.Fatalf("upstream error leaked to client: %s", body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("completed event missing: %s", body)
	}
}

func TestProxyRetriesResponseFailedWithoutExposingIt(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w,
				"event: response.created\ndata: {\"type\":\"response.created\"}\n\n"+
					"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"Selected model is at capacity. Please try a different model.\"}}}\n\n",
			)
			return
		}
		writeCompletedStream(w, "recovered")
	}))
	defer upstream.Close()

	body, _ := makeHeldRequest(t, newTestProxy(t, upstream.URL, true))
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if strings.Contains(body, "response.failed") || strings.Contains(body, "capacity") {
		t.Fatalf("failed attempt leaked to client: %s", body)
	}
	if !strings.Contains(body, "recovered") {
		t.Fatalf("successful attempt missing: %s", body)
	}
}

func TestProxyRetriesErrorEventWithoutExposingIt(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w,
				"event: error\ndata: {\"error\":{\"type\":\"upstream_error\",\"message\":\"Upstream request failed\"}}\n\n",
			)
			return
		}
		writeCompletedStream(w, "recovered-from-error-event")
	}))
	defer upstream.Close()

	body, _ := makeHeldRequest(t, newTestProxy(t, upstream.URL, true))
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if strings.Contains(body, "Upstream request failed") || strings.Contains(body, "event: error") {
		t.Fatalf("error event leaked to client: %s", body)
	}
	if !strings.Contains(body, "recovered-from-error-event") {
		t.Fatalf("successful attempt missing: %s", body)
	}
}

func TestProxyRetriesTransportDisconnect(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer does not support hijacking")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		writeCompletedStream(w, "recovered-from-disconnect")
	}))
	defer upstream.Close()

	body, _ := makeHeldRequest(t, newTestProxy(t, upstream.URL, true))
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if !strings.Contains(body, "recovered-from-disconnect") {
		t.Fatalf("successful attempt missing: %s", body)
	}
}

func TestProxyRetriesStreamWithoutTerminalEvent(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"discard me\"}\n\n")
			return
		}
		writeCompletedStream(w, "kept")
	}))
	defer upstream.Close()

	body, _ := makeHeldRequest(t, newTestProxy(t, upstream.URL, true))
	if strings.Contains(body, "discard me") {
		t.Fatalf("incomplete attempt leaked to client: %s", body)
	}
	if !strings.Contains(body, "kept") {
		t.Fatalf("successful response missing: %s", body)
	}
}

func TestProxyRotatesAfterStreamIdleTimeout(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-request.Context().Done()
			return
		}
		writeCompletedStream(w, "after-idle")
	}))
	defer upstream.Close()

	handler := newTestProxy(t, upstream.URL, true)
	handler.cfg.StreamIdleTimeout = 30 * time.Millisecond
	body, _ := makeHeldRequest(t, handler)
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if !strings.Contains(body, "after-idle") {
		t.Fatalf("successful response missing: %s", body)
	}
}

func TestDisabledProxyPassesErrorThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "direct error", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	body, status := makeHeldRequest(t, newTestProxy(t, upstream.URL, false))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if !strings.Contains(body, "direct error") {
		t.Fatalf("direct response missing: %s", body)
	}
}

func TestControlTogglePersists(t *testing.T) {
	handler := newTestProxy(t, "http://127.0.0.1:1", true)
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPut, server.URL+controlPrefix+"/api/enabled", strings.NewReader(`{"enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if handler.Enabled() {
		t.Fatal("handler remains enabled")
	}

	reloaded, err := newEnableController(true, handler.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Enabled() {
		t.Fatal("persisted setting was not loaded")
	}
}

func TestClientCancellationStopsRetryLoop(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	defer upstream.Close()

	handler := newTestProxy(t, upstream.URL, true)
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	_, _ = response.Body.Read(buffer)
	cancel()
	response.Body.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(handler.registry.list()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled request remained active")
}

func newTestProxy(t *testing.T, upstreamURL string, enabled bool) *Proxy {
	t.Helper()
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	cfg := Config{
		ListenAddr:            "127.0.0.1:0",
		UpstreamURL:           parsed,
		ProtectedPaths:        []string{"/responses"},
		Enabled:               enabled,
		StateFile:             tempDir + "/state.json",
		SpoolDir:              tempDir + "/spool",
		KeepaliveInterval:     10 * time.Millisecond,
		RetryMinInterval:      time.Millisecond,
		RetryMaxInterval:      5 * time.Millisecond,
		RetryJitterRatio:      0,
		ResponseHeaderTimeout: time.Second,
		StreamIdleTimeout:     200 * time.Millisecond,
		AttemptMaxDuration:    time.Second,
		MaxRequestBodyBytes:   1024 * 1024,
		MaxAttemptBodyBytes:   1024 * 1024,
		MaxSSELineBytes:       64 * 1024,
		MaxIdleConnsPerHost:   16,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	return handler
}

func makeHeldRequest(t *testing.T, handler http.Handler) (string, int) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Post(
		server.URL+"/responses",
		"application/json",
		bytes.NewBufferString(`{"model":"gpt-test","stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), response.StatusCode
}

func writeCompletedStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w,
		"event: response.created\ndata: {\"type\":\"response.created\"}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":"+quote(text)+"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
	)
}

func quote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
