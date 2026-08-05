package handler

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
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
	openAIStreamHoldRequestStateRefresh openAIStreamHoldReason = "request_state_refresh"
)

type openAIStreamHoldObservation struct {
	AccountID          int64
	UpstreamStatusCode int
	LastError          string
}

type openAIStreamHoldController struct {
	configured        bool
	runtimeEnabled    func() bool
	tracker           service.OpenAIStreamHoldTracker
	minRetryInterval  time.Duration
	maxRetryInterval  time.Duration
	retryJitterRatio  float64
	maxDuration       time.Duration
	nextRetryInterval time.Duration
	waitCount         int

	mu                  sync.Mutex
	trackingOpMu        sync.Mutex
	state               service.OpenAIStreamHoldState
	finished            bool
	heartbeatCancel     context.CancelFunc
	trackingWriteFailed bool
	requestStateRefresh bool
}

func newOpenAIStreamHoldController(
	cfg *config.Config,
	stream bool,
	runtimeEnabled func() bool,
	tracker service.OpenAIStreamHoldTracker,
	state service.OpenAIStreamHoldState,
) *openAIStreamHoldController {
	controller := &openAIStreamHoldController{tracker: tracker, state: state}
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
	controller.nextRetryInterval = hold.MinRetryInterval
	if controller.state.RequestStartedAt.IsZero() {
		controller.state.RequestStartedAt = time.Now().UTC()
	}
	return controller
}

func newOpenAIStreamHoldState(
	c *gin.Context,
	requestStartedAt time.Time,
	userID int64,
	apiKey *service.APIKey,
	platform string,
	model string,
) service.OpenAIStreamHoldState {
	state := service.OpenAIStreamHoldState{
		UserID:           userID,
		Platform:         platform,
		Model:            model,
		RequestStartedAt: requestStartedAt.UTC(),
	}
	if apiKey != nil {
		state.APIKeyID = apiKey.ID
		if apiKey.GroupID != nil {
			groupID := *apiKey.GroupID
			state.GroupID = &groupID
		}
	}
	if c == nil || c.Request == nil {
		return state
	}
	state.RequestPath = c.Request.URL.Path
	state.RequestID, _ = c.Request.Context().Value(ctxkey.RequestID).(string)
	state.ClientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	state.LeaseID = state.ClientRequestID
	return state
}

func (h *openAIStreamHoldController) Enabled() bool {
	return h != nil && h.configured && h.runtimeEnabled != nil && h.runtimeEnabled()
}

func (h *openAIStreamHoldController) Wait(
	c *gin.Context,
	reqLog *zap.Logger,
	reason openAIStreamHoldReason,
	retryAfter time.Duration,
	observation openAIStreamHoldObservation,
) bool {
	if !h.Enabled() {
		h.logDisabled(reqLog, reason)
		h.finish(reqLog, "disabled")
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
		heldSince := h.heldSince()
		if heldSince.IsZero() {
			heldSince = time.Now()
		}
		remaining := time.Until(heldSince.Add(h.maxDuration))
		if remaining <= 0 {
			h.logDeadline(reqLog, reason)
			h.finish(reqLog, "deadline")
			return false
		}
		if delay >= remaining {
			delay = remaining
			deadlineLimited = true
		}
	}

	h.waitCount++
	firstHold := h.updateTrackingState(
		service.OpenAIStreamHoldPhaseHolding,
		reason,
		delay,
		observation,
	)
	if reqLog != nil {
		reqLog.Warn("openai.stream_hold_waiting",
			zap.String("reason", string(reason)),
			zap.Int("hold_cycle", h.waitCount),
			zap.Duration("retry_delay", delay),
			zap.Duration("held_for", h.heldFor()),
			zap.Duration("max_duration", h.maxDuration),
			zap.Bool("downstream_response_deferred", true),
		)
	}
	if firstHold {
		if reqLog != nil {
			reqLog.Info("openai.stream_hold_tracking_started",
				zap.String("reason", string(reason)),
				zap.String("phase", service.OpenAIStreamHoldPhaseHolding),
				zap.Bool("redis_lease", h.tracker != nil),
			)
		}
	}
	h.persist(reqLog, "state_update")
	if firstHold {
		h.startHeartbeat(reqLog)
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
					zap.Duration("held_for", h.heldFor()),
					zap.Error(ctx.Err()),
				)
			}
			h.finish(reqLog, "client_canceled")
			return false
		case <-timer.C:
			if !h.Enabled() {
				h.logDisabled(reqLog, reason)
				h.finish(reqLog, "disabled")
				return false
			}
			if deadlineLimited {
				h.logDeadline(reqLog, reason)
				h.finish(reqLog, "deadline")
				return false
			}
			h.advanceBackoff()
			h.mu.Lock()
			h.requestStateRefresh = true
			h.mu.Unlock()
			h.updateTrackingState(
				service.OpenAIStreamHoldPhaseRetrying,
				reason,
				0,
				observation,
			)
			h.persist(reqLog, "retrying")
			if reqLog != nil {
				reqLog.Info("openai.stream_hold_retrying",
					zap.String("reason", string(reason)),
					zap.Int("hold_cycle", h.waitCount),
					zap.Duration("held_for", h.heldFor()),
					zap.Duration("next_retry_interval", h.nextRetryInterval),
				)
			}
			return true
		case <-stateCheckTicker.C:
			if !h.Enabled() {
				h.logDisabled(reqLog, reason)
				h.finish(reqLog, "disabled")
				return false
			}
		}
	}
}

func (h *openAIStreamHoldController) RequestStateRefreshNeeded() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requestStateRefresh && !h.finished
}

func (h *openAIStreamHoldController) RequestStateRefreshed(
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	platform string,
) {
	if h == nil || apiKey == nil {
		return
	}
	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return
	}
	h.requestStateRefresh = false
	h.state.Platform = platform
	h.state.APIKeyID = apiKey.ID
	h.state.GroupID = nil
	if apiKey.GroupID != nil {
		groupID := *apiKey.GroupID
		h.state.GroupID = &groupID
	}
	h.state.UpdatedAt = time.Now().UTC()
	h.mu.Unlock()
	h.persist(reqLog, "request_state_refreshed")
}

func (h *openAIStreamHoldController) jitteredRetryInterval(base time.Duration) time.Duration {
	if base <= 0 || h.retryJitterRatio <= 0 {
		return base
	}
	factor := 1 + (rand.Float64()*2-1)*h.retryJitterRatio
	return max(time.Duration(float64(base)*factor), time.Millisecond)
}

func (h *openAIStreamHoldController) Recovered(reqLog *zap.Logger, accountID int64) {
	if h == nil || !h.configured || h.waitCount == 0 {
		return
	}
	h.setAccountID(accountID)
	if reqLog != nil {
		reqLog.Info("openai.stream_hold_recovered",
			zap.Int64("account_id", accountID),
			zap.Int("hold_cycles", h.waitCount),
			zap.Duration("held_for", h.heldFor()),
			zap.Bool("downstream_response_deferred", true),
		)
	}
	h.finish(reqLog, "recovered")
}

func (h *openAIStreamHoldController) Retrying(reqLog *zap.Logger, accountID int64) {
	if h == nil || accountID <= 0 || h.waitCount == 0 {
		return
	}
	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return
	}
	value := accountID
	h.state.AccountID = &value
	h.state.Phase = service.OpenAIStreamHoldPhaseRetrying
	h.state.UpdatedAt = time.Now().UTC()
	h.state.RetryDelayMs = 0
	h.state.NextRetryAt = nil
	h.mu.Unlock()

	h.persist(reqLog, "upstream_attempt")
	if reqLog != nil {
		reqLog.Info("openai.stream_hold_upstream_attempt",
			zap.Int64("account_id", accountID),
			zap.Int("hold_cycle", h.waitCount),
			zap.Duration("held_for", h.heldFor()),
		)
	}
}

func (h *openAIStreamHoldController) Close(reqLog *zap.Logger, requestContext context.Context) {
	if h == nil || h.waitCount == 0 {
		return
	}
	if requestContext != nil && requestContext.Err() != nil {
		h.finish(reqLog, "client_canceled")
		return
	}
	if !h.Enabled() {
		h.finish(reqLog, "disabled")
		return
	}
	h.finish(reqLog, "terminal")
}

func (h *openAIStreamHoldController) logDisabled(reqLog *zap.Logger, reason openAIStreamHoldReason) {
	if h == nil || h.waitCount == 0 || reqLog == nil {
		return
	}
	reqLog.Info("openai.stream_hold_disabled",
		zap.String("reason", string(reason)),
		zap.Int("hold_cycles", h.waitCount),
		zap.Duration("held_for", h.heldFor()),
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
		zap.Duration("held_for", h.heldFor()),
		zap.Duration("max_duration", h.maxDuration),
	)
}

func (h *openAIStreamHoldController) updateTrackingState(
	phase string,
	reason openAIStreamHoldReason,
	retryDelay time.Duration,
	observation openAIStreamHoldObservation,
) bool {
	if h == nil {
		return false
	}
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished {
		return false
	}
	firstHold := h.state.HeldSince.IsZero()
	if firstHold {
		h.state.HeldSince = now
	}
	h.state.Phase = phase
	h.state.Reason = string(reason)
	h.state.HoldCycle = h.waitCount
	h.state.UpdatedAt = now
	h.state.RetryDelayMs = max(retryDelay.Milliseconds(), 0)
	if retryDelay > 0 {
		nextRetryAt := now.Add(retryDelay)
		h.state.NextRetryAt = &nextRetryAt
	} else {
		h.state.NextRetryAt = nil
	}
	if observation.AccountID > 0 {
		accountID := observation.AccountID
		h.state.AccountID = &accountID
	}
	if observation.UpstreamStatusCode > 0 {
		statusCode := observation.UpstreamStatusCode
		h.state.LastUpstreamStatusCode = &statusCode
	}
	if strings.TrimSpace(observation.LastError) != "" {
		h.state.LastError = observation.LastError
	}
	return firstHold
}

func (h *openAIStreamHoldController) setAccountID(accountID int64) {
	if h == nil || accountID <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	value := accountID
	h.state.AccountID = &value
	h.state.UpdatedAt = time.Now().UTC()
}

func (h *openAIStreamHoldController) heldSince() time.Time {
	if h == nil {
		return time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state.HeldSince
}

func (h *openAIStreamHoldController) heldFor() time.Duration {
	heldSince := h.heldSince()
	if heldSince.IsZero() {
		return 0
	}
	return max(time.Since(heldSince), 0)
}

func (h *openAIStreamHoldController) startHeartbeat(reqLog *zap.Logger) {
	if h == nil || h.tracker == nil || h.tracker.HeartbeatInterval() <= 0 {
		return
	}
	h.mu.Lock()
	if h.heartbeatCancel != nil || h.finished {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.heartbeatCancel = cancel
	h.mu.Unlock()

	go func() {
		ticker := time.NewTicker(h.tracker.HeartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.persist(reqLog, "heartbeat")
			}
		}
	}()
}

func (h *openAIStreamHoldController) persist(reqLog *zap.Logger, operation string) {
	if h == nil || h.tracker == nil {
		return
	}
	h.trackingOpMu.Lock()
	defer h.trackingOpMu.Unlock()

	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return
	}
	state := h.state
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), h.tracker.OperationTimeout())
	err := h.tracker.Upsert(ctx, &state)
	cancel()

	h.mu.Lock()
	previouslyFailed := h.trackingWriteFailed
	h.trackingWriteFailed = err != nil
	h.mu.Unlock()
	if err != nil {
		if !previouslyFailed && reqLog != nil {
			reqLog.Error("openai.stream_hold_tracking_failed",
				zap.String("operation", operation),
				zap.Error(err),
			)
		}
		return
	}
	if previouslyFailed && reqLog != nil {
		reqLog.Info("openai.stream_hold_tracking_restored",
			zap.String("operation", operation),
		)
	}
}

func (h *openAIStreamHoldController) finish(reqLog *zap.Logger, outcome string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.finished || h.state.HeldSince.IsZero() {
		h.mu.Unlock()
		return
	}
	h.finished = true
	leaseID := h.state.LeaseID
	heartbeatCancel := h.heartbeatCancel
	h.heartbeatCancel = nil
	h.mu.Unlock()

	if heartbeatCancel != nil {
		heartbeatCancel()
	}

	var deleteErr error
	if h.tracker != nil {
		h.trackingOpMu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), h.tracker.OperationTimeout())
		deleteErr = h.tracker.Delete(ctx, leaseID)
		cancel()
		h.trackingOpMu.Unlock()
	}
	if reqLog != nil {
		fields := []zap.Field{
			zap.String("outcome", outcome),
			zap.Int("hold_cycles", h.waitCount),
			zap.Duration("held_for", h.heldFor()),
		}
		if deleteErr != nil {
			fields = append(fields, zap.Error(deleteErr))
			reqLog.Error("openai.stream_hold_tracking_finished", fields...)
		} else {
			reqLog.Info("openai.stream_hold_tracking_finished", fields...)
		}
	}
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
