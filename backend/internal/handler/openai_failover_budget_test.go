package handler

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNextOpenAIAccountFailoverSwitchCountPausedDoesNotConsumeBudget(t *testing.T) {
	for _, code := range []int{http.StatusPaymentRequired, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		err := &service.UpstreamFailoverError{
			StatusCode:        code,
			Reason:            service.GrokAccountSchedulingPausedReason,
			NextAccountAction: service.NextAccountRetry,
		}
		next, ok := nextOpenAIAccountFailoverSwitchCount(10, 3, err)
		require.True(t, ok, "status %d", code)
		require.Equal(t, 10, next, "status %d", code)
	}
}

func TestNextOpenAIAccountFailoverSwitchCountGenericConsumesBudget(t *testing.T) {
	err := &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway, NextAccountAction: service.NextAccountRetry}
	next, ok := nextOpenAIAccountFailoverSwitchCount(2, 3, err)
	require.True(t, ok)
	require.Equal(t, 3, next)
	next, ok = nextOpenAIAccountFailoverSwitchCount(3, 3, err)
	require.False(t, ok)
	require.Equal(t, 3, next)
}
