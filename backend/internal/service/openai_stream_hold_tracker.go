package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	OpenAIStreamHoldPhaseHolding  = "holding"
	OpenAIStreamHoldPhaseRetrying = "retrying"

	openAIStreamHoldLastErrorMaxBytes = 512
)

var ErrOpenAIStreamHoldTrackerUnavailable = errors.New("stream hold tracker is unavailable")

type OpenAIStreamHoldState struct {
	LeaseID         string `json:"-"`
	RequestID       string `json:"request_id"`
	ClientRequestID string `json:"client_request_id,omitempty"`

	UserID    int64  `json:"user_id"`
	APIKeyID  int64  `json:"api_key_id"`
	GroupID   *int64 `json:"group_id,omitempty"`
	AccountID *int64 `json:"account_id,omitempty"`

	Platform    string `json:"platform"`
	Model       string `json:"model"`
	RequestPath string `json:"request_path"`

	Phase     string `json:"phase"`
	Reason    string `json:"reason"`
	HoldCycle int    `json:"hold_cycle"`

	RequestStartedAt time.Time  `json:"request_started_at"`
	HeldSince        time.Time  `json:"held_since"`
	UpdatedAt        time.Time  `json:"updated_at"`
	RetryDelayMs     int64      `json:"retry_delay_ms"`
	NextRetryAt      *time.Time `json:"next_retry_at,omitempty"`

	LastUpstreamStatusCode *int   `json:"last_upstream_status_code,omitempty"`
	LastError              string `json:"last_error,omitempty"`
}

type OpenAIStreamHoldFilter struct {
	Platform  string
	GroupID   *int64
	AccountID *int64
}

type OpenAIStreamHoldSummary struct {
	ActiveCount   int            `json:"active_count"`
	HoldingCount  int            `json:"holding_count"`
	RetryingCount int            `json:"retrying_count"`
	OldestHeldMs  int64          `json:"oldest_held_ms"`
	AverageHeldMs int64          `json:"average_held_ms"`
	ByReason      map[string]int `json:"by_reason"`
}

type OpenAIStreamHoldSnapshot struct {
	Enabled   bool                     `json:"enabled"`
	Holds     []*OpenAIStreamHoldState `json:"holds"`
	Summary   OpenAIStreamHoldSummary  `json:"summary"`
	Timestamp time.Time                `json:"timestamp"`
}

type OpenAIStreamHoldStore interface {
	Upsert(ctx context.Context, state *OpenAIStreamHoldState, leaseTTL time.Duration) error
	Delete(ctx context.Context, leaseID string) error
	List(ctx context.Context) ([]*OpenAIStreamHoldState, error)
}

type OpenAIStreamHoldTracker interface {
	Upsert(ctx context.Context, state *OpenAIStreamHoldState) error
	Delete(ctx context.Context, leaseID string) error
	List(ctx context.Context, filter OpenAIStreamHoldFilter) (*OpenAIStreamHoldSnapshot, error)
	HeartbeatInterval() time.Duration
	OperationTimeout() time.Duration
}

type openAIStreamHoldTracker struct {
	store             OpenAIStreamHoldStore
	settingService    *SettingService
	heartbeatInterval time.Duration
	leaseTTL          time.Duration
	operationTimeout  time.Duration
}

func NewOpenAIStreamHoldTracker(
	store OpenAIStreamHoldStore,
	settingService *SettingService,
	cfg *config.Config,
) OpenAIStreamHoldTracker {
	tracker := &openAIStreamHoldTracker{store: store, settingService: settingService}
	if cfg != nil {
		hold := cfg.Gateway.OpenAIStreamHold
		tracker.heartbeatInterval = hold.TrackingHeartbeatInterval
		tracker.leaseTTL = hold.TrackingLeaseTTL
		tracker.operationTimeout = hold.TrackingOperationTimeout
	}
	return tracker
}

func (t *openAIStreamHoldTracker) Upsert(ctx context.Context, state *OpenAIStreamHoldState) error {
	if t == nil || t.store == nil {
		return ErrOpenAIStreamHoldTrackerUnavailable
	}
	if state == nil || strings.TrimSpace(state.LeaseID) == "" {
		return errors.New("stream hold lease_id is required")
	}
	if strings.TrimSpace(state.RequestID) == "" {
		return errors.New("stream hold request_id is required")
	}
	if state.Phase != OpenAIStreamHoldPhaseHolding && state.Phase != OpenAIStreamHoldPhaseRetrying {
		return errors.New("stream hold phase is invalid")
	}

	copyState := *state
	copyState.LeaseID = strings.TrimSpace(copyState.LeaseID)
	copyState.RequestID = strings.TrimSpace(copyState.RequestID)
	copyState.ClientRequestID = strings.TrimSpace(copyState.ClientRequestID)
	copyState.Platform = strings.TrimSpace(copyState.Platform)
	copyState.Model = strings.TrimSpace(copyState.Model)
	copyState.RequestPath = strings.TrimSpace(copyState.RequestPath)
	copyState.Reason = strings.TrimSpace(copyState.Reason)
	copyState.LastError = truncateOpenAIStreamHoldText(copyState.LastError, openAIStreamHoldLastErrorMaxBytes)
	copyState.RequestStartedAt = copyState.RequestStartedAt.UTC()
	copyState.HeldSince = copyState.HeldSince.UTC()
	copyState.UpdatedAt = copyState.UpdatedAt.UTC()
	if copyState.NextRetryAt != nil {
		nextRetryAt := copyState.NextRetryAt.UTC()
		copyState.NextRetryAt = &nextRetryAt
	}
	return t.store.Upsert(ctx, &copyState, t.leaseTTL)
}

func (t *openAIStreamHoldTracker) Delete(ctx context.Context, leaseID string) error {
	if t == nil || t.store == nil {
		return ErrOpenAIStreamHoldTrackerUnavailable
	}
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return nil
	}
	return t.store.Delete(ctx, leaseID)
}

func (t *openAIStreamHoldTracker) List(ctx context.Context, filter OpenAIStreamHoldFilter) (*OpenAIStreamHoldSnapshot, error) {
	if t == nil || t.store == nil {
		return nil, ErrOpenAIStreamHoldTrackerUnavailable
	}
	holds, err := t.store.List(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	platform := strings.TrimSpace(filter.Platform)
	filtered := make([]*OpenAIStreamHoldState, 0, len(holds))
	summary := OpenAIStreamHoldSummary{ByReason: make(map[string]int)}
	var totalHeldMs int64
	for _, hold := range holds {
		if hold == nil {
			continue
		}
		if platform != "" && hold.Platform != platform {
			continue
		}
		if filter.GroupID != nil && (hold.GroupID == nil || *hold.GroupID != *filter.GroupID) {
			continue
		}
		if filter.AccountID != nil && (hold.AccountID == nil || *hold.AccountID != *filter.AccountID) {
			continue
		}

		filtered = append(filtered, hold)
		heldMs := now.Sub(hold.HeldSince).Milliseconds()
		if heldMs < 0 {
			heldMs = 0
		}
		totalHeldMs += heldMs
		if heldMs > summary.OldestHeldMs {
			summary.OldestHeldMs = heldMs
		}
		switch hold.Phase {
		case OpenAIStreamHoldPhaseHolding:
			summary.HoldingCount++
		case OpenAIStreamHoldPhaseRetrying:
			summary.RetryingCount++
		}
		summary.ByReason[hold.Reason]++
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].HeldSince.Before(filtered[j].HeldSince)
	})
	summary.ActiveCount = len(filtered)
	if summary.ActiveCount > 0 {
		summary.AverageHeldMs = totalHeldMs / int64(summary.ActiveCount)
	}

	enabled := false
	if t.settingService != nil {
		enabled = t.settingService.IsOpenAIStreamHoldEnabled()
	}
	return &OpenAIStreamHoldSnapshot{
		Enabled:   enabled,
		Holds:     filtered,
		Summary:   summary,
		Timestamp: now,
	}, nil
}

func (t *openAIStreamHoldTracker) HeartbeatInterval() time.Duration {
	if t == nil {
		return 0
	}
	return t.heartbeatInterval
}

func (t *openAIStreamHoldTracker) OperationTimeout() time.Duration {
	if t == nil {
		return 0
	}
	return t.operationTimeout
}

func truncateOpenAIStreamHoldText(value string, maxBytes int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
