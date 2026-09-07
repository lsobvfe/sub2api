package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const testAPIKey = "test-stream-hold-key"

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

func TestProxyLogsResponseFailedTerminal(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w,
				"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"Selected model is at capacity.\"}}}\n\n",
			)
			return
		}
		writeCompletedStream(w, "recovered")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	body, _ := makeHeldRequest(t, newTestProxyWithLogger(t, upstream.URL, true, logger))
	if !strings.Contains(body, "recovered") {
		t.Fatalf("successful response missing: %s", body)
	}

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		if record["msg"] != "hold_attempt_finished" || record["attempt"] != float64(1) {
			continue
		}
		if record["outcome"] != "failure" {
			t.Fatalf("outcome = %v, want failure", record["outcome"])
		}
		if record["terminal_outcome"] != "failure" {
			t.Fatalf("terminal_outcome = %v, want failure", record["terminal_outcome"])
		}
		if record["terminal_type"] != "response.failed" {
			t.Fatalf("terminal_type = %v, want response.failed", record["terminal_type"])
		}
		if record["failure_class"] != "sse_terminal_failure" {
			t.Fatalf("failure_class = %v, want sse_terminal_failure", record["failure_class"])
		}
		return
	}
	t.Fatal("response.failed terminal diagnostic log missing")
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

func TestProxyHoldsAnthropicMessagesStream(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer upstream.Close()

	handler := newTestProxy(t, upstream.URL, true)
	handler.cfg.ProtectedPaths = []string{"/v1/messages"}
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-test","stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.StatusCode, body)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
	if !strings.Contains(string(body), "event: message_stop") {
		t.Fatalf("Anthropic terminal event missing: %s", body)
	}
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("Anthropic content missing: %s", body)
	}
	if strings.Contains(string(body), "response.metadata") {
		t.Fatalf("Responses keepalive leaked into Messages stream: %s", body)
	}
}

func TestProxyHoldsChatCompletionsAfterServiceUnavailable(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, `{"message":"Service temporarily unavailable","type":"api_error"}`, http.StatusServiceUnavailable)
			return
		}
		writeChatCompletedStream(w, "recovered")
	}))
	defer upstream.Close()

	body, status := makeHeldRequestTo(
		t,
		newTestProxy(t, upstream.URL, true),
		"/v1/chat/completions",
		`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"test"}],"stream":true}`,
	)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if strings.Contains(body, "Service temporarily unavailable") || strings.Contains(body, `"type":"api_error"`) {
		t.Fatalf("upstream 503 leaked to client: %s", body)
	}
	if strings.Contains(body, "response.metadata") {
		t.Fatalf("Responses keepalive leaked into Chat Completions stream: %s", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("DONE count = %d, want 1; body = %s", strings.Count(body, "data: [DONE]"), body)
	}
	if !strings.Contains(body, "recovered") {
		t.Fatalf("successful Chat Completions stream missing: %s", body)
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
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
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

func TestWriteKeepaliveEmitsResponseMetadata(t *testing.T) {
	recorder := httptest.NewRecorder()
	if err := streamProtocolOpenAIResponses.writeKeepalive(recorder, http.NewResponseController(recorder), "waiting"); err != nil {
		t.Fatalf("write keepalive: %v", err)
	}

	line, _, found := strings.Cut(recorder.Body.String(), "\n")
	if !found {
		t.Fatalf("SSE frame missing line terminator: %q", recorder.Body.String())
	}
	data, found := strings.CutPrefix(line, "data: ")
	if !found {
		t.Fatalf("SSE frame = %q, want data frame", line)
	}

	var payload struct {
		Type       string            `json:"type"`
		StreamHold map[string]string `json:"stream_hold"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode keepalive payload: %v", err)
	}
	if payload.Type != "response.metadata" {
		t.Fatalf("type = %q, want response.metadata", payload.Type)
	}
	if payload.StreamHold["phase"] != "waiting" {
		t.Fatalf("phase = %q, want waiting", payload.StreamHold["phase"])
	}
}

func TestWriteKeepaliveEmitsCommentForNonResponsesProtocols(t *testing.T) {
	for _, protocol := range []streamProtocol{
		streamProtocolAnthropicMessages,
		streamProtocolOpenAIChatCompletions,
	} {
		t.Run(protocol.String(), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if err := protocol.writeKeepalive(recorder, http.NewResponseController(recorder), "waiting"); err != nil {
				t.Fatalf("write keepalive: %v", err)
			}
			frame := recorder.Body.String()
			if frame != ": stream-hold waiting\n\n" {
				t.Fatalf("keepalive frame = %q", frame)
			}
			if strings.Contains(frame, "response.metadata") {
				t.Fatalf("Responses metadata leaked into %s", protocol)
			}
		})
	}
}

func newTestProxy(t *testing.T, upstreamURL string, enabled bool) *Proxy {
	t.Helper()
	return newTestProxyWithLogger(t, upstreamURL, enabled, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newTestProxyWithLogger(t *testing.T, upstreamURL string, enabled bool, logger *slog.Logger) *Proxy {
	t.Helper()
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	cfg := Config{
		ListenAddr:            "127.0.0.1:0",
		UpstreamURL:           parsed,
		ProtectedPaths:        allStreamProtocolPaths(),
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
	handler, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testAPIKey))
	if err := handler.keyRegistry.Set(hex.EncodeToString(sum[:]), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	return handler
}

func makeHeldRequest(t *testing.T, handler http.Handler) (string, int) {
	return makeHeldRequestTo(t, handler, "/responses", `{"model":"gpt-test","stream":true}`)
}

func makeHeldRequestTo(t *testing.T, handler http.Handler, path, requestBody string) (string, int) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+path,
		bytes.NewBufferString(requestBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(responseBody), response.StatusCode
}

func writeCompletedStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w,
		"event: response.created\ndata: {\"type\":\"response.created\"}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":"+quote(text)+"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
	)
}

func writeChatCompletedStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w,
		"data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":"+quote(text)+"},\"finish_reason\":null}]}\n\n"+
			"data: [DONE]\n\n",
	)
}

func quote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
