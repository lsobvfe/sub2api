package handler

import (
	"context"
	"errors"
	"fmt"
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
	enabled           bool
	keepaliveInterval time.Duration
	minRetryInterval  time.Duration
	maxRetryInterval  time.Duration
	retryJitterRatio  float64
	maxDuration       time.Duration
	startedAt         time.Time
	nextRetryInterval time.Duration
	waitCount         int
	keepaliveCount    int
}

func newOpenAIStreamHoldController(cfg *config.Config, stream bool) *openAIStreamHoldController {
	controller := &openAIStreamHoldController{}
	if cfg == nil || !stream || !cfg.Gateway.OpenAIStreamHold.Enabled {
		return controller
	}
	hold := cfg.Gateway.OpenAIStreamHold
	controller.enabled = true
	controller.keepaliveInterval = hold.KeepaliveInterval
	controller.minRetryInterval = hold.MinRetryInterval
	controller.maxRetryInterval = hold.MaxRetryInterval
	controller.retryJitterRatio = hold.RetryJitterRatio
	controller.maxDuration = hold.MaxDuration
	controller.startedAt = time.Now()
	controller.nextRetryInterval = hold.MinRetryInterval
	return controller
}

func (h *openAIStreamHoldController) Enabled() bool {
	return h != nil && h.enabled
}

func (h *openAIStreamHoldController) Wait(
	c *gin.Context,
	reqLog *zap.Logger,
	reason openAIStreamHoldReason,
	retryAfter time.Duration,
	streamStarted *bool,
) bool {
	if !h.Enabled() || c == nil || c.Request == nil || c.Writer == nil {
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
	if err := h.writeKeepalive(c, reqLog, reason, streamStarted); err != nil {
		return false
	}
	if reqLog != nil {
		reqLog.Warn("openai.stream_hold_waiting",
			zap.String("reason", string(reason)),
			zap.Int("hold_cycle", h.waitCount),
			zap.Duration("retry_delay", delay),
			zap.Duration("held_for", time.Since(h.startedAt)),
			zap.Duration("max_duration", h.maxDuration),
		)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	ticker := time.NewTicker(h.keepaliveInterval)
	defer ticker.Stop()

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
		case <-ticker.C:
			if err := h.writeKeepalive(c, reqLog, reason, streamStarted); err != nil {
				return false
			}
		}
	}
}

func (h *openAIStreamHoldController) writeKeepalive(
	c *gin.Context,
	reqLog *zap.Logger,
	reason openAIStreamHoldReason,
	streamStarted *bool,
) error {
	if err := writeOpenAIStreamHoldKeepalive(c, streamStarted); err != nil {
		if reqLog != nil {
			reqLog.Info("openai.stream_hold_client_write_failed",
				zap.String("reason", string(reason)),
				zap.Int("hold_cycle", h.waitCount),
				zap.Duration("held_for", time.Since(h.startedAt)),
				zap.Error(err),
			)
		}
		return err
	}
	h.keepaliveCount++
	if reqLog != nil {
		reqLog.Debug("openai.stream_hold_keepalive",
			zap.String("reason", string(reason)),
			zap.Int("hold_cycle", h.waitCount),
			zap.Int("keepalive_count", h.keepaliveCount),
			zap.Duration("held_for", time.Since(h.startedAt)),
		)
	}
	return nil
}

func (h *openAIStreamHoldController) jitteredRetryInterval(base time.Duration) time.Duration {
	if base <= 0 || h.retryJitterRatio <= 0 {
		return base
	}
	factor := 1 + (rand.Float64()*2-1)*h.retryJitterRatio
	return max(time.Duration(float64(base)*factor), time.Millisecond)
}

func (h *openAIStreamHoldController) Recovered(reqLog *zap.Logger, accountID int64) {
	if !h.Enabled() || h.waitCount == 0 || reqLog == nil {
		return
	}
	reqLog.Info("openai.stream_hold_recovered",
		zap.Int64("account_id", accountID),
		zap.Int("hold_cycles", h.waitCount),
		zap.Int("keepalive_count", h.keepaliveCount),
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

func (h *openAIStreamHoldController) logDeadline(reqLog *zap.Logger, reason openAIStreamHoldReason) {
	if reqLog == nil {
		return
	}
	reqLog.Warn("openai.stream_hold_deadline_reached",
		zap.String("reason", string(reason)),
		zap.Int("hold_cycles", h.waitCount),
		zap.Int("keepalive_count", h.keepaliveCount),
		zap.Duration("held_for", time.Since(h.startedAt)),
		zap.Duration("max_duration", h.maxDuration),
	)
}

func writeOpenAIStreamHoldKeepalive(c *gin.Context, streamStarted *bool) error {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return errors.New("streaming response writer does not support flushing")
	}
	if streamStarted != nil && !*streamStarted {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		*streamStarted = true
	}
	if _, err := fmt.Fprint(c.Writer, string(SSEPingFormatComment)); err != nil {
		return err
	}
	flusher.Flush()
	return nil
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
