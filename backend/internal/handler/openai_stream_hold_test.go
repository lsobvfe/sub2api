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
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func testOpenAIStreamHoldConfig() config.GatewayOpenAIStreamHoldConfig {
	return config.GatewayOpenAIStreamHoldConfig{
		Enabled:          true,
		MinRetryInterval: 20 * time.Millisecond,
		MaxRetryInterval: 40 * time.Millisecond,
		RetryJitterRatio: 0,
		MaxDuration:      0,
	}
}

func TestOpenAIStreamHoldDisabledForNonStreamingRequests(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	require.False(t, newOpenAIStreamHoldController(
		cfg,
		false,
		func() bool { return true },
		nil,
		service.OpenAIStreamHoldState{},
	).Enabled())
}

func TestNewOpenAIStreamHoldStateUsesClientRequestIDAsLeaseIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestContext := context.WithValue(context.Background(), ctxkey.RequestID, "request-shared")
	requestContext = context.WithValue(requestContext, ctxkey.ClientRequestID, "client-request-unique")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)

	state := newOpenAIStreamHoldState(
		c,
		time.Now(),
		10,
		&service.APIKey{ID: 11},
		service.PlatformOpenAI,
		"gpt-5.1",
	)

	require.Equal(t, "client-request-unique", state.LeaseID)
	require.Equal(t, "request-shared", state.RequestID)
	require.Equal(t, "client-request-unique", state.ClientRequestID)
}

func TestOpenAIStreamHoldWaitDefersResponseAndRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	hold := newOpenAIStreamHoldController(cfg, true, func() bool { return true }, nil, service.OpenAIStreamHoldState{})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	require.True(t, hold.Wait(c, nil, openAIStreamHoldNoAccount, 0, openAIStreamHoldObservation{}))
	require.False(t, c.Writer.Written())
	require.False(t, recorder.Flushed)
	require.Empty(t, recorder.Header())
	require.Empty(t, recorder.Body.String())
	require.Equal(t, 40*time.Millisecond, hold.nextRetryInterval)
}

func TestOpenAIStreamHoldWaitStopsOnClientCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	hold := newOpenAIStreamHoldController(cfg, true, func() bool { return true }, nil, service.OpenAIStreamHoldState{})
	ctx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	done := make(chan bool, 1)
	go func() {
		done <- hold.Wait(c, nil, openAIStreamHoldNoAccount, 0, openAIStreamHoldObservation{})
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
	hold := newOpenAIStreamHoldController(cfg, true, enabled.Load, nil, service.OpenAIStreamHoldState{})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	done := make(chan bool, 1)
	go func() {
		done <- hold.Wait(c, nil, openAIStreamHoldNoAccount, 0, openAIStreamHoldObservation{})
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

type openAIStreamHoldTrackerSpy struct {
	mu      sync.Mutex
	states  []service.OpenAIStreamHoldState
	deletes []string
}

func (s *openAIStreamHoldTrackerSpy) Upsert(_ context.Context, state *service.OpenAIStreamHoldState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyState := *state
	s.states = append(s.states, copyState)
	return nil
}

func (s *openAIStreamHoldTrackerSpy) Delete(_ context.Context, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, requestID)
	return nil
}

func (s *openAIStreamHoldTrackerSpy) List(
	context.Context,
	service.OpenAIStreamHoldFilter,
) (*service.OpenAIStreamHoldSnapshot, error) {
	return nil, nil
}

func (s *openAIStreamHoldTrackerSpy) HeartbeatInterval() time.Duration {
	return 5 * time.Millisecond
}

func (s *openAIStreamHoldTrackerSpy) OperationTimeout() time.Duration {
	return 100 * time.Millisecond
}

func (s *openAIStreamHoldTrackerSpy) snapshot() ([]service.OpenAIStreamHoldState, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.OpenAIStreamHoldState(nil), s.states...), append([]string(nil), s.deletes...)
}

func TestOpenAIStreamHoldTracksActiveLifecycleUntilRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIStreamHold: testOpenAIStreamHoldConfig()}}
	tracker := &openAIStreamHoldTrackerSpy{}
	hold := newOpenAIStreamHoldController(
		cfg,
		true,
		func() bool { return true },
		tracker,
		service.OpenAIStreamHoldState{
			LeaseID:         "lease-hold-1",
			RequestID:       "request-hold-1",
			ClientRequestID: "client-request-hold-1",
			UserID:          10,
			APIKeyID:        11,
			Platform:        service.PlatformOpenAI,
			Model:           "gpt-5.1",
			RequestPath:     "/v1/responses",
		},
	)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	require.True(t, hold.Wait(
		c,
		nil,
		openAIStreamHoldUpstreamUnavailable,
		0,
		openAIStreamHoldObservation{
			AccountID:          42,
			UpstreamStatusCode: http.StatusBadGateway,
			LastError:          "temporary upstream failure",
		},
	))

	require.Eventually(t, func() bool {
		states, _ := tracker.snapshot()
		return len(states) >= 2
	}, time.Second, 5*time.Millisecond)
	states, deletes := tracker.snapshot()
	require.Empty(t, deletes)
	require.Equal(t, service.OpenAIStreamHoldPhaseHolding, states[0].Phase)
	require.Equal(t, openAIStreamHoldUpstreamUnavailable, openAIStreamHoldReason(states[0].Reason))
	require.Equal(t, int64(42), *states[0].AccountID)
	require.Equal(t, http.StatusBadGateway, *states[0].LastUpstreamStatusCode)
	require.Equal(t, service.OpenAIStreamHoldPhaseRetrying, states[len(states)-1].Phase)

	hold.Retrying(nil, 43)
	states, _ = tracker.snapshot()
	require.Equal(t, service.OpenAIStreamHoldPhaseRetrying, states[len(states)-1].Phase)
	require.Equal(t, int64(43), *states[len(states)-1].AccountID)

	hold.Recovered(nil, 43)
	require.Eventually(t, func() bool {
		_, currentDeletes := tracker.snapshot()
		return len(currentDeletes) == 1
	}, time.Second, 5*time.Millisecond)
	_, deletes = tracker.snapshot()
	require.Equal(t, []string{"lease-hold-1"}, deletes)
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
		0,
		0,
		0,
	}, upstream.responseHeaderTimeouts())
	require.NotContains(t, string(responseBody), string(SSEPingFormatComment))
	require.Contains(t, string(responseBody), `"type":"response.completed"`)
	require.Contains(t, string(responseBody), `"id":"resp_hold_recovered"`)
	require.NotContains(t, string(responseBody), `"type":"response.failed"`)
}
