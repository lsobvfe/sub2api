package service

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	openAIRawRelayExtraKey       = "openai_raw_relay"
	openAIRawRelayRequestBodyKey = "openai_raw_relay_request_body"
)

// IsOpenAIRawRelayEnabled reports whether an OpenAI API-key account should
// preserve the client's HTTP request and replace only the upstream target and auth.
func (a *Account) IsOpenAIRawRelayEnabled() bool {
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeAPIKey || a.Extra == nil {
		return false
	}
	enabled, ok := a.Extra[openAIRawRelayExtraKey].(bool)
	return ok && enabled
}

// SetOpenAIRawRelayRequestBody snapshots the bytes read at ingress, before any
// compatibility policy or model mapping can rewrite them.
func SetOpenAIRawRelayRequestBody(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	c.Set(openAIRawRelayRequestBodyKey, bytes.Clone(body))
}

func openAIRawRelayRequestBody(c *gin.Context, fallback []byte) []byte {
	if c != nil {
		if raw, ok := c.Get(openAIRawRelayRequestBodyKey); ok {
			if body, ok := raw.([]byte); ok {
				return body
			}
		}
	}
	return fallback
}

var openAIRawRelayHopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func stripOpenAIRawRelayHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				header.Del(name)
			}
		}
	}
	for _, name := range openAIRawRelayHopByHopHeaders {
		header.Del(name)
	}
}

func (s *OpenAIGatewayService) buildUpstreamRequestOpenAIRawRelay(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) (*http.Request, error) {
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	targetURL := appendOpenAIResponsesRequestPathSuffix(
		buildOpenAIResponsesURL(validatedURL),
		openAIResponsesRequestPathSuffix(c),
	)
	method := http.MethodPost
	if c != nil && c.Request != nil && c.Request.Method != "" {
		method = c.Request.Method
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	if c != nil && c.Request != nil {
		req.Header = c.Request.Header.Clone()
		if c.Request.URL != nil {
			req.URL.RawQuery = c.Request.URL.RawQuery
		}
	}
	stripOpenAIRawRelayHopByHopHeaders(req.Header)
	req.Header.Del("Content-Length")
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")

	authHeaders, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, err
	}
	for key, values := range authHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	return req, nil
}
