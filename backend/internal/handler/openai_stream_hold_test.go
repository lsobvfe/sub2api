//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
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
		Enabled:                true,
		UpstreamAttemptTimeout: 80 * time.Millisecond,
		MinRetryInterval:       20 * time.Millisecond,
		MaxRetryInterval:       40 * time.Millisecond,
		RetryJitterRatio:       0,
		MaxDuration:            0,
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

type openAIStreamHoldCapacityMode string

const (
	openAIStreamHoldCapacityHTTP openAIStreamHoldCapacityMode = "http_400"
	openAIStreamHoldCapacitySSE  openAIStreamHoldCapacityMode = "sse_failed"
)

type openAIStreamHoldCapacityUpstream struct {
	service.HTTPUpstream
	mu    sync.Mutex
	mode  openAIStreamHoldCapacityMode
	calls int
}

func (u *openAIStreamHoldCapacityUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	call := u.calls
	u.mu.Unlock()

	if call == 1 {
		switch u.mode {
		case openAIStreamHoldCapacityHTTP:
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
					"X-Request-Id": []string{"rid-capacity-http"},
				},
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"message":"Selected model is at capacity. Please try a different model.","type":"invalid_request_error"}}`,
				)),
			}, nil
		case openAIStreamHoldCapacitySSE:
			body := "event: response.created\n" +
				`data: {"type":"response.created","response":{"id":"resp_capacity"}}` + "\n\n" +
				"event: response.failed\n" +
				`data: {"type":"response.failed","error":{"message":"Selected model is at capacity. Please try a different model.","type":"invalid_request_error"}}` + "\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
					"X-Request-Id": []string{"rid-capacity-sse"},
				},
				Body: io.NopCloser(strings.NewReader(body)),
			}, nil
		}
	}

	body := `data: {"type":"response.output_text.delta","item_id":"msg_capacity","output_index":0,"content_index":0,"delta":"ok"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_capacity_recovered","object":"response","model":"gpt-5.1","status":"completed","output":[{"type":"message","id":"msg_capacity","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (u *openAIStreamHoldCapacityUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

type openAIStreamHoldSilentBody struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	closed chan struct{}
	once   sync.Once
}

func newOpenAIStreamHoldSilentBody() *openAIStreamHoldSilentBody {
	reader, writer := io.Pipe()
	return &openAIStreamHoldSilentBody{
		reader: reader,
		writer: writer,
		closed: make(chan struct{}),
	}
}

func (b *openAIStreamHoldSilentBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *openAIStreamHoldSilentBody) Close() error {
	var err error
	b.once.Do(func() {
		close(b.closed)
		err = errors.Join(b.reader.Close(), b.writer.Close())
	})
	return err
}

type openAIStreamHoldSilentUpstream struct {
	service.HTTPUpstream
	mu        sync.Mutex
	calls     int
	firstBody *openAIStreamHoldSilentBody
}

func (u *openAIStreamHoldSilentUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	if u.calls == 1 {
		u.firstBody = newOpenAIStreamHoldSilentBody()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       u.firstBody,
		}, nil
	}
	return openAIStreamHoldSuccessResponse("resp_silent_recovered"), nil
}

func (u *openAIStreamHoldSilentUpstream) snapshot() (int, *openAIStreamHoldSilentBody) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, u.firstBody
}

type openAIStreamHoldGroupAwareAccountRepo struct {
	service.AccountRepository
	mu               sync.Mutex
	groupIDs         []int64
	initialAccount   service.Account
	refreshedAccount service.Account
}

func (r *openAIStreamHoldGroupAwareAccountRepo) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	group, _ := ctx.Value(ctxkey.Group).(*service.Group)
	groupID := int64(0)
	if group != nil {
		groupID = group.ID
	}
	r.mu.Lock()
	r.groupIDs = append(r.groupIDs, groupID)
	r.mu.Unlock()
	if platform != service.PlatformOpenAI {
		return nil, nil
	}
	switch groupID {
	case 4201:
		return []service.Account{r.initialAccount}, nil
	case 4202:
		return []service.Account{r.refreshedAccount}, nil
	default:
		return nil, nil
	}
}

func (r *openAIStreamHoldGroupAwareAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for _, account := range []service.Account{r.initialAccount, r.refreshedAccount} {
		if id == account.ID {
			copyAccount := account
			return &copyAccount, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r *openAIStreamHoldGroupAwareAccountRepo) calls() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.groupIDs...)
}

type openAIStreamHoldGroupSwitchUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
}

func (u *openAIStreamHoldGroupSwitchUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	if accountID == 1 {
		return &http.Response{
			StatusCode: 520,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html>temporary upstream failure</html>")),
		}, nil
	}
	return openAIStreamHoldSuccessResponse("resp_group_recovered"), nil
}

func (u *openAIStreamHoldGroupSwitchUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func openAIStreamHoldSuccessResponse(responseID string) *http.Response {
	body := `data: {"type":"response.output_text.delta","item_id":"msg_hold","output_index":0,"content_index":0,"delta":"ok"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"` + responseID + `","object":"response","model":"gpt-5.1","status":"completed","output":[{"type":"message","id":"msg_hold","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestOpenAIResponsesStreamHoldRotatesSilentUpstreamAttemptWithoutDisconnectingClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIStreamHoldSilentUpstream{}
	accounts := []service.Account{{
		ID:          1,
		Name:        "silent-account",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 0,
		Credentials: map[string]any{"access_token": "token-1"},
	}}
	handler := newOpenAIResponsesFailoverTestHandlerWithAccountsAndConfig(
		t,
		upstream,
		accounts,
		func(cfg *config.Config) {
			cfg.Gateway.OpenAIStreamHold = testOpenAIStreamHoldConfig()
		},
	)

	groupID := int64(3131)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID:      99,
			GroupID: &groupID,
			Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
			User:    &service.User{ID: 100},
		})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
		handler.Responses(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	startedAt := time.Now()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Post(
		server.URL+"/v1/responses",
		"application/json",
		bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	calls, firstBody := upstream.snapshot()
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", responseBody)
	require.Equal(t, 2, calls)
	require.NotNil(t, firstBody)
	select {
	case <-firstBody.closed:
	default:
		t.Fatal("silent upstream body was not closed at the attempt deadline")
	}
	require.GreaterOrEqual(t, time.Since(startedAt), testOpenAIStreamHoldConfig().UpstreamAttemptTimeout)
	require.Contains(t, string(responseBody), `"id":"resp_silent_recovered"`)
	require.NotContains(t, string(responseBody), "first_output_timeout")
	require.NotContains(t, string(responseBody), "Upstream request failed")
}

func TestOpenAIResponsesStreamHoldRefreshesAPIKeyGroupBeforeRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	accountRepo := &openAIStreamHoldGroupAwareAccountRepo{
		initialAccount: service.Account{
			ID:          1,
			Name:        "stale-group-account",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Credentials: map[string]any{"access_token": "token-1"},
		},
		refreshedAccount: service.Account{
			ID:          2,
			Name:        "refreshed-group-account",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Credentials: map[string]any{"access_token": "token-2"},
		},
	}
	upstream := &openAIStreamHoldGroupSwitchUpstream{}
	handler := newOpenAIResponsesFailoverTestHandlerWithRepositoryAndConfig(
		t,
		upstream,
		accountRepo,
		func(cfg *config.Config) {
			cfg.Gateway.OpenAIStreamHold = testOpenAIStreamHoldConfig()
		},
	)
	refreshedGroupID := int64(4202)
	handler.apiKeyService = service.NewAPIKeyService(
		&openAIResponsesRuntimeAPIKeyRepo{
			loader: func(int) *service.APIKey {
				return &service.APIKey{
					ID:      99,
					UserID:  100,
					GroupID: &refreshedGroupID,
					Status:  service.StatusActive,
					User:    &service.User{ID: 100, Status: service.StatusActive},
					Group: &service.Group{
						ID:       refreshedGroupID,
						Platform: service.PlatformOpenAI,
						Status:   service.StatusActive,
					},
				}
			},
		},
		nil, nil, nil, nil, nil, handler.cfg,
	)

	initialGroupID := int64(4201)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		initialGroup := &service.Group{
			ID:       initialGroupID,
			Platform: service.PlatformOpenAI,
			Status:   service.StatusActive,
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, initialGroup))
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID:      99,
			UserID:  100,
			GroupID: &initialGroupID,
			Status:  service.StatusActive,
			Group:   initialGroup,
			User:    &service.User{ID: 100, Status: service.StatusActive},
		})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
		handler.Responses(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	resp, err := (&http.Client{Timeout: 2 * time.Second}).Post(
		server.URL+"/v1/responses",
		"application/json",
		bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", responseBody)
	groupCalls := accountRepo.calls()
	require.NotEmpty(t, groupCalls)
	require.Equal(t, initialGroupID, groupCalls[0])
	require.Equal(t, refreshedGroupID, groupCalls[len(groupCalls)-1])
	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Contains(t, string(responseBody), `"id":"resp_group_recovered"`)
	require.NotContains(t, string(responseBody), "No available accounts")
}

func TestOpenAIResponsesStreamHoldRecoversFromCapacityWithoutExposingFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, mode := range []openAIStreamHoldCapacityMode{
		openAIStreamHoldCapacityHTTP,
		openAIStreamHoldCapacitySSE,
	} {
		t.Run(string(mode), func(t *testing.T) {
			upstream := &openAIStreamHoldCapacityUpstream{mode: mode}
			accounts := []service.Account{{
				ID:          1,
				Name:        "capacity-account",
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeOAuth,
				Status:      service.StatusActive,
				Schedulable: true,
				Concurrency: 0,
				Credentials: map[string]any{
					"access_token":          "token-1",
					"pool_mode":             true,
					"pool_mode_retry_count": 0,
				},
			}}
			handler := newOpenAIResponsesFailoverTestHandlerWithAccountsAndConfig(
				t,
				upstream,
				accounts,
				func(cfg *config.Config) {
					cfg.Gateway.OpenAIStreamHold = testOpenAIStreamHoldConfig()
				},
			)
			handler.maxAccountSwitches = 0

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

			startedAt := time.Now()
			req, err := http.NewRequest(
				http.MethodPost,
				server.URL+"/v1/responses",
				bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`),
			)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			responseBody, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", responseBody)
			require.Equal(t, 2, upstream.callCount())
			require.GreaterOrEqual(t, time.Since(startedAt), testOpenAIStreamHoldConfig().MinRetryInterval)
			require.Contains(t, string(responseBody), `"type":"response.completed"`)
			require.Contains(t, string(responseBody), `"id":"resp_capacity_recovered"`)
			require.NotContains(t, string(responseBody), "Upstream request failed")
			require.NotContains(t, string(responseBody), "Selected model is at capacity")
			require.NotContains(t, string(responseBody), `"type":"response.failed"`)
		})
	}
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
