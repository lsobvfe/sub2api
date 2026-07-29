//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesStreamHoldEnabledIsRouteScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIStreamHold: config.GatewayOpenAIStreamHoldConfig{Enabled: true},
			},
		},
	}

	for _, path := range []string{
		"/v1/responses",
		"/openai/v1/responses",
		"/backend-api/codex/responses/compact",
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		require.True(t, svc.openAIResponsesStreamHoldEnabled(c), path)
	}

	for _, path := range []string{
		"/v1/chat/completions",
		"/openai/v1/messages",
		"/v1/images/generations",
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		require.False(t, svc.openAIResponsesStreamHoldEnabled(c), path)
	}
}
