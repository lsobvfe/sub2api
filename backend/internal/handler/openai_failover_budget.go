package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// nextOpenAIAccountFailoverSwitchCount decides whether the request may select
// another account after a failover-classified upstream failure.
//
// Design:
//   - If the error already removed the account from the schedulable pool
//     (Grok 402 payment / 429 quota / 503 service-unavailable), do NOT spend the
//     generic switch budget. Keep scanning until SelectAccount reports no
//     remaining candidates (success or true empty pool).
//   - Status-code fallback covers older construction sites that set StatusCode
//     but not Reason.
//   - Other retryable failures still use maxSwitches as a safety bound against
//     pathological non-progressing loops. failedAccountIDs already prevent
//     re-picking the same account.
func nextOpenAIAccountFailoverSwitchCount(
	switchCount int,
	maxSwitches int,
	failoverErr *service.UpstreamFailoverError,
) (int, bool) {
	if failoverErr != nil && !failoverErr.ShouldRetryNextAccount() {
		return switchCount, false
	}
	if isAccountAlreadyOutOfPool(failoverErr) {
		return switchCount, true
	}
	if maxSwitches > 0 && switchCount >= maxSwitches {
		return switchCount, false
	}
	return switchCount + 1, true
}

func isAccountAlreadyOutOfPool(failoverErr *service.UpstreamFailoverError) bool {
	if failoverErr == nil {
		return false
	}
	if failoverErr.IsGrokAccountSchedulingPaused() {
		return true
	}
	switch failoverErr.StatusCode {
	case http.StatusPaymentRequired, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	default:
		return false
	}
}
