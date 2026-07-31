//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAIStreamHoldStoreStub struct {
	upserted *OpenAIStreamHoldState
	deleted  string
	holds    []*OpenAIStreamHoldState
}

func (s *openAIStreamHoldStoreStub) Upsert(_ context.Context, state *OpenAIStreamHoldState, _ time.Duration) error {
	copyState := *state
	s.upserted = &copyState
	return nil
}

func (s *openAIStreamHoldStoreStub) Delete(_ context.Context, requestID string) error {
	s.deleted = requestID
	return nil
}

func (s *openAIStreamHoldStoreStub) List(context.Context) ([]*OpenAIStreamHoldState, error) {
	return s.holds, nil
}

func TestOpenAIStreamHoldTrackerNormalizesFiltersAndSummarizes(t *testing.T) {
	now := time.Now().UTC()
	groupID := int64(7)
	accountID := int64(8)
	store := &openAIStreamHoldStoreStub{
		holds: []*OpenAIStreamHoldState{
			{
				RequestID: "older", Platform: PlatformOpenAI, GroupID: &groupID, AccountID: &accountID,
				Phase: OpenAIStreamHoldPhaseHolding, Reason: "upstream_unavailable", HeldSince: now.Add(-4 * time.Second),
			},
			{
				RequestID: "newer", Platform: PlatformOpenAI, GroupID: &groupID,
				Phase: OpenAIStreamHoldPhaseRetrying, Reason: "upstream_unavailable", HeldSince: now.Add(-2 * time.Second),
			},
			{
				RequestID: "other", Platform: PlatformGrok,
				Phase: OpenAIStreamHoldPhaseHolding, Reason: "no_available_account", HeldSince: now.Add(-time.Second),
			},
		},
	}
	settings := NewSettingService(nil, nil)
	settings.openAIStreamHoldEnabled.Store(true)
	tracker := NewOpenAIStreamHoldTracker(store, settings, &config.Config{
		Gateway: config.GatewayConfig{
			OpenAIStreamHold: config.GatewayOpenAIStreamHoldConfig{
				TrackingHeartbeatInterval: 10 * time.Second,
				TrackingLeaseTTL:          45 * time.Second,
				TrackingOperationTimeout:  2 * time.Second,
			},
		},
	})

	snapshot, err := tracker.List(context.Background(), OpenAIStreamHoldFilter{
		Platform: PlatformOpenAI,
		GroupID:  &groupID,
	})
	require.NoError(t, err)
	require.True(t, snapshot.Enabled)
	require.Len(t, snapshot.Holds, 2)
	require.Equal(t, "older", snapshot.Holds[0].RequestID)
	require.Equal(t, 2, snapshot.Summary.ActiveCount)
	require.Equal(t, 1, snapshot.Summary.HoldingCount)
	require.Equal(t, 1, snapshot.Summary.RetryingCount)
	require.Equal(t, 2, snapshot.Summary.ByReason["upstream_unavailable"])
	require.GreaterOrEqual(t, snapshot.Summary.OldestHeldMs, int64(3900))
	require.GreaterOrEqual(t, snapshot.Summary.AverageHeldMs, int64(2900))
}

func TestOpenAIStreamHoldTrackerSanitizesWritesAndDeletes(t *testing.T) {
	store := &openAIStreamHoldStoreStub{}
	tracker := NewOpenAIStreamHoldTracker(store, nil, &config.Config{
		Gateway: config.GatewayConfig{
			OpenAIStreamHold: config.GatewayOpenAIStreamHoldConfig{
				TrackingHeartbeatInterval: 10 * time.Second,
				TrackingLeaseTTL:          45 * time.Second,
				TrackingOperationTimeout:  2 * time.Second,
			},
		},
	})
	now := time.Now()
	require.NoError(t, tracker.Upsert(context.Background(), &OpenAIStreamHoldState{
		LeaseID:          " lease-1 ",
		RequestID:        " request-1 ",
		Phase:            OpenAIStreamHoldPhaseHolding,
		HeldSince:        now,
		UpdatedAt:        now,
		RequestStartedAt: now,
		LastError:        string(make([]byte, openAIStreamHoldLastErrorMaxBytes+100)),
	}))
	require.NotNil(t, store.upserted)
	require.Equal(t, "lease-1", store.upserted.LeaseID)
	require.Equal(t, "request-1", store.upserted.RequestID)
	require.LessOrEqual(t, len(store.upserted.LastError), openAIStreamHoldLastErrorMaxBytes)

	require.NoError(t, tracker.Delete(context.Background(), " lease-1 "))
	require.Equal(t, "lease-1", store.deleted)
}
