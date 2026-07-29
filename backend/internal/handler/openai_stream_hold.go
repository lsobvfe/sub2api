package handler

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type openAIStreamHoldReason string

const (
	openAIStreamHoldNoAccount           openAIStreamHoldReason = "no_available_account"
	openAIStreamHoldUserConcurrency     openAIStreamHoldReason = "user_concurrency"
	openAIStreamHoldAccountConcurrency  openAIStreamHoldReason = "account_concurrency"
	openAIStreamHoldUpstreamRetryable   openAIStreamHoldReason = "upstream_retryable"
	openAIStreamHoldUpstreamRateLimit   openAIStreamHoldReason = "upstream_rate_limit"
	openAIStreamHoldUpstreamUnavailable openAIStreamHoldReason = "upstream_unavailable"
)

type openAIStreamHoldController struct {
	configured        bool
	runtimeEnabled    func() bool
	minRetryInterval  time.Duration
	maxRetryInterval  time.Duration
	retryJitterRatio  float64
	maxDuration       time.Duration
	startedAt         time.Time
	nextRetryInterval time.Duration
	waitCount         int
}

func newOpenAIStreamHoldController(
	cfg *config.Config,
	stream bool,
	runtimeEnabled func() bool,
) *openAIStreamHoldController {
	controller := &openAIStreamHoldController{}
	if cfg == nil || !stream || runtimeEnabled == nil {
		return controller
	}
	hold := cfg.Gateway.OpenAIStreamHold
	controller.configured = true
	controller.runtimeEnabled = runtimeEnabled
	controller.minRetryInterval = hold.MinRetryInterval
	controller.maxRetryInterval = hold.MaxRetryInterval
	controller.retryJitterRatio = hold.RetryJitterRatio
	controller.maxDuration = hold.MaxDuration
	controller.startedAt = time.Now()
	controller.nextRetryInterval = hold.MinRetryInterval
	return controller
}

func (h *openAIStreamHoldController) Enabled() bool {
	return h != nil && h.configured && h.runtimeEnabled != nil && h.runtimeEnabled()
}

func (h *openAIStreamHoldController) Wait(
	c *gin.Context,
	reqLog *zap.Logger,
	reason openAIStreamHoldReason,
	retryAfter time.Duration,
) bool {
	if !h.Enabled() {
		h.logDisabled(reqLog, reason)
		return false
	}
	if c == nil || c.Request == nil {
		return false
	}
	ctx := c.Request.Context()
	if ctx.Err() != nil {
		return false
	}

	delay := h.nextRetryInterval
	if retryAfter > delay {
		delay = retryAfter
	} else {
		delay = h.jitteredRetryInterval(delay)
	}
	if delay <= 0 {
		delay = h.minRetryInterval
	}

	deadlineLimited := false
	if h.maxDuration > 0 {
		remaining := time.Until(h.startedAt.Add(h.maxDuration))
		if remaining <= 0 {
			h.logDeadline(reqLog, reason)
			return false
		}
		if delay >= remaining {
			delay = remaining
			deadlineLimited = true
		}
	}

	h.waitCount++
	if reqLog != nil {
		reqLog.Warn("openai.stream_hold_waiting",
			zap.String("reason", string(reason)),
			zap.Int("hold_cycle", h.waitCount),
			zap.Duration("retry_delay", delay),
			zap.Duration("held_for", time.Since(h.startedAt)),
			zap.Duration("max_duration", h.maxDuration),
			zap.Bool("downstream_response_deferred", true),
		)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	stateCheckInterval := min(h.minRetryInterval, delay)
	if stateCheckInterval <= 0 {
		stateCheckInterval = delay
	}
	stateCheckTicker := time.NewTicker(stateCheckInterval)
	defer stateCheckTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			if reqLog != nil {
				reqLog.Info("openai.stream_hold_canceled",
					zap.String("reason", string(reason)),
					zap.Int("hold_cycle", h.waitCount),
					zap.Duration("held_for", time.Since(h.startedAt)),
					zap.Error(ctx.Err()),
				)
			}
			return false
		case <-timer.C:
			if !h.Enabled() {
				h.logDisabled(reqLog, reason)
				return false
			}
			if deadlineLimited {
				h.logDeadline(reqLog, reason)
				return false
			}
			h.advanceBackoff()
			if reqLog != nil {
				reqLog.Info("openai.stream_hold_retrying",
					zap.String("reason", string(reason)),
					zap.Int("hold_cycle", h.waitCount),
					zap.Duration("held_for", time.Since(h.startedAt)),
					zap.Duration("next_retry_interval", h.nextRetryInterval),
				)
			}
			return true
		case <-stateCheckTicker.C:
			if !h.Enabled() {
				h.logDisabled(reqLog, reason)
				return false
			}
		}
	}
}

func (h *openAIStreamHoldController) jitteredRetryInterval(base time.Duration) time.Duration {
	if base <= 0 || h.retryJitterRatio <= 0 {
		return base
	}
	factor := 1 + (rand.Float64()*2-1)*h.retryJitterRatio
	return max(time.Duration(float64(base)*factor), time.Millisecond)
}

func (h *openAIStreamHoldController) Recovered(reqLog *zap.Logger, accountID int64) {
	if h == nil || !h.configured || h.waitCount == 0 || reqLog == nil {
		return
	}
	reqLog.Info("openai.stream_hold_recovered",
		zap.Int64("account_id", accountID),
		zap.Int("hold_cycles", h.waitCount),
		zap.Duration("held_for", time.Since(h.startedAt)),
		zap.Bool("downstream_response_deferred", true),
	)
}

func (h *openAIStreamHoldController) logDisabled(reqLog *zap.Logger, reason openAIStreamHoldReason) {
	if h == nil || h.waitCount == 0 || reqLog == nil {
		return
	}
	reqLog.Info("openai.stream_hold_disabled",
		zap.String("reason", string(reason)),
		zap.Int("hold_cycles", h.waitCount),
		zap.Duration("held_for", time.Since(h.startedAt)),
	)
}

func (h *openAIStreamHoldController) advanceBackoff() {
	next := h.nextRetryInterval * 2
	if next < h.minRetryInterval {
		next = h.minRetryInterval
	}
	if next > h.maxRetryInterval {
		next = h.maxRetryInterval
	}
	h.nextRetryInterval = next
}

func (h *OpenAIGatewayHandler) openAIStreamHoldEnabled() bool {
	return h != nil && h.gatewayService != nil && h.gatewayService.OpenAIStreamHoldEnabled()
}

func (h *openAIStreamHoldController) logDeadline(reqLog *zap.Logger, reason openAIStreamHoldReason) {
	if reqLog == nil {
		return
	}
	reqLog.Warn("openai.stream_hold_deadline_reached",
		zap.String("reason", string(reason)),
		zap.Int("hold_cycles", h.waitCount),
		zap.Duration("held_for", time.Since(h.startedAt)),
		zap.Duration("max_duration", h.maxDuration),
	)
}

func openAIStreamHoldFailoverReason(err *service.UpstreamFailoverError) (openAIStreamHoldReason, bool) {
	if err == nil || err.IsOpenAIRequestBodyTooLarge() || service.IsOpenAISilentRefusalErrorBody(err.ResponseBody) {
		return "", false
	}
	if err.StatusCode == http.StatusTooManyRequests || err.StatusCode == 529 {
		return openAIStreamHoldUpstreamRateLimit, true
	}
	if err.StatusCode >= 500 || err.ClientStatusCode >= 500 {
		return openAIStreamHoldUpstreamUnavailable, true
	}
	if err.ShouldRetryNextAccount() {
		return openAIStreamHoldUpstreamRetryable, true
	}
	return "", false
}

func openAIStreamHoldRetryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return time.Duration(seconds) * time.Second
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	return max(time.Until(retryAt), 0)
}

func openAIStreamHoldContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
