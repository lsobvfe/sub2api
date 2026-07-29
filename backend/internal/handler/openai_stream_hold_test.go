//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func testOpenAIStreamHoldConfig() config.GatewayOpenAIStreamHoldConfig {
	return config.GatewayOpenAIStreamHoldConfig{
		Enabled:               true,
		KeepaliveInterval:     5 * time.Millisecond,
		ResponseHeaderTimeout: 15 * time.Millisecond,
		MinRetryInterval:      20 * time.Millisecond,
		MaxRetryInterval:      40 * time.Millisecond,
		RetryJitterRatio:      0,
		MaxDuration:           0,
	}
}

func TestOpenAIStreamHoldDisabledForNonStreamingRequests(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	require.False(t, newOpenAIStreamHoldController(cfg, false, func() bool { return true }).Enabled())
}

func TestOpenAIStreamHoldWaitSendsKeepaliveAndRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	hold := newOpenAIStreamHoldController(cfg, true, func() bool { return true })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	streamStarted := false

	require.True(t, hold.Wait(c, nil, openAIStreamHoldNoAccount, 0, &streamStarted))
	require.True(t, streamStarted)
	require.True(t, recorder.Flushed)
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	require.GreaterOrEqual(t, strings.Count(recorder.Body.String(), string(SSEPingFormatComment)), 1)
	require.Equal(t, 40*time.Millisecond, hold.nextRetryInterval)
}

func TestOpenAIStreamHoldWaitStopsOnClientCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	hold := newOpenAIStreamHoldController(cfg, true, func() bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	streamStarted := false

	done := make(chan bool, 1)
	go func() {
		done <- hold.Wait(c, nil, openAIStreamHoldNoAccount, 0, &streamStarted)
	}()
	time.Sleep(8 * time.Millisecond)
	cancel()

	select {
	case retry := <-done:
		require.False(t, retry)
	case <-time.After(time.Second):
		t.Fatal("stream hold did not stop after client cancellation")
	}
}

func TestOpenAIStreamHoldWaitStopsWhenRuntimeIsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	var enabled atomic.Bool
	enabled.Store(true)
	hold := newOpenAIStreamHoldController(cfg, true, enabled.Load)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	streamStarted := false

	done := make(chan bool, 1)
	go func() {
		done <- hold.Wait(c, nil, openAIStreamHoldNoAccount, 0, &streamStarted)
	}()
	time.Sleep(8 * time.Millisecond)
	enabled.Store(false)

	select {
	case retry := <-done:
		require.False(t, retry)
	case <-time.After(time.Second):
		t.Fatal("stream hold did not stop after runtime disable")
	}
}

type openAIStreamHoldRecoveryUpstream struct {
	service.HTTPUpstream
	mu             sync.Mutex
	calls          int
	headerTimeouts []time.Duration
}

func (u *openAIStreamHoldRecoveryUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	call := u.calls
	u.headerTimeouts = append(u.headerTimeouts, service.HTTPUpstreamResponseHeaderTimeoutFromContext(req.Context()))
	u.mu.Unlock()

	if call <= 2 {
		return &http.Response{
			StatusCode: 520,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html>temporary upstream failure</html>")),
		}, nil
	}
	body := `data: {"type":"response.output_text.delta","item_id":"msg_hold","output_index":0,"content_index":0,"delta":"ok"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_hold_recovered","object":"response","model":"gpt-5.1","status":"completed","output":[{"type":"message","id":"msg_hold","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (u *openAIStreamHoldRecoveryUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *openAIStreamHoldRecoveryUpstream) responseHeaderTimeouts() []time.Duration {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]time.Duration(nil), u.headerTimeouts...)
}

func TestOpenAIResponsesStreamHoldRecoversAfterFailoverExhaustion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIStreamHoldRecoveryUpstream{}
	handler := newOpenAIResponsesFailoverTestHandlerWithConfig(t, upstream, func(cfg *config.Config) {
		cfg.Gateway.OpenAIStreamHold = testOpenAIStreamHoldConfig()
	})
	handler.maxAccountSwitches = 1
	require.True(t, handler.gatewayService.OpenAIStreamHoldEnabled())

	groupID := int64(3131)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID:      99,
			GroupID: &groupID,
			Group: &service.Group{
				ID:       groupID,
				Platform: service.PlatformOpenAI,
			},
			User: &service.User{ID: 100},
		})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
		handler.Responses(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v1/responses",
		bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`),
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s calls=%d timeouts=%v", responseBody, upstream.callCount(), upstream.responseHeaderTimeouts())
	require.Equal(t, 3, upstream.callCount())
	require.Equal(t, []time.Duration{
		15 * time.Millisecond,
		15 * time.Millisecond,
		15 * time.Millisecond,
	}, upstream.responseHeaderTimeouts())
	require.Contains(t, string(responseBody), string(SSEPingFormatComment))
	require.Contains(t, string(responseBody), `"type":"response.completed"`)
	require.Contains(t, string(responseBody), `"id":"resp_hold_recovered"`)
	require.NotContains(t, string(responseBody), `"type":"response.failed"`)
}
