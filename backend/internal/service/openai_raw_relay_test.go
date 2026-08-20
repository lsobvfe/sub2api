package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type rawRelayNetworkUpstream struct {
	client *http.Client
}

func (u rawRelayNetworkUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u rawRelayNetworkUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.client.Do(req)
}

func TestAccount_IsOpenAIRawRelayEnabled(t *testing.T) {
	require.True(t, (&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"openai_raw_relay": true},
	}).IsOpenAIRawRelayEnabled())
	require.False(t, (&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"openai_raw_relay": true},
	}).IsOpenAIRawRelayEnabled())
}

func TestOpenAIGatewayService_RawRelayPreservesIngressRequestAndReplacesAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type receivedRequest struct {
		method string
		url    string
		header http.Header
		body   []byte
	}
	received := make(chan receivedRequest, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		received <- receivedRequest{
			method: req.Method,
			url:    req.URL.RequestURI(),
			header: req.Header.Clone(),
			body:   body,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req_raw")
		_, _ = io.WriteString(w, `{"id":"resp_raw","model":"gpt-5.4","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstreamServer.Close()

	originalBody := []byte("{\n  \"model\": \"gpt-5.4\",\n  \"stream\": false,\n  \"input\": [{\"type\":\"message\",\"id\":\"msg_keep_me\"}]\n}")
	mutatedBody := []byte(`{"model":"mapped-model","stream":false,"input":[]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses?client_trace=keep", bytes.NewReader(originalBody))
	c.Request.Header.Set("Authorization", "Bearer sub2api-client-key")
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.146.0")
	c.Request.Header.Set("Originator", "codex_cli_rs")
	c.Request.Header.Set("X-Custom-Codex", "preserve-me")
	c.Request.Header.Set("Connection", "keep-alive, X-Remove-Me")
	c.Request.Header.Set("X-Remove-Me", "hop-by-hop")
	SetOpenAIRawRelayRequestBody(c, originalBody)

	svc := &OpenAIGatewayService{
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled:           false,
			AllowInsecureHTTP: true,
		}}},
		httpUpstream: rawRelayNetworkUpstream{client: upstreamServer.Client()},
	}
	account := &Account{
		ID:          901,
		Name:        "raw-relay",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-upstream", "base_url": upstreamServer.URL + "/v1"},
		Extra:       map[string]any{"openai_raw_relay": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, mutatedBody)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	actual := <-received
	require.Equal(t, "/v1/responses?client_trace=keep", actual.url)
	require.Equal(t, http.MethodPost, actual.method)
	require.Equal(t, originalBody, actual.body)
	require.Equal(t, "Bearer sk-upstream", actual.header.Get("Authorization"))
	require.Equal(t, "codex_cli_rs/0.146.0", actual.header.Get("User-Agent"))
	require.Equal(t, "codex_cli_rs", actual.header.Get("Originator"))
	require.Equal(t, "preserve-me", actual.header.Get("X-Custom-Codex"))
	require.Empty(t, actual.header.Get("Connection"))
	require.Empty(t, actual.header.Get("X-Remove-Me"))
}
