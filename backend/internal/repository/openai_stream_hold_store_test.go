//go:build unit

package repository

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStreamHoldStoreLifecycleAndLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := &openAIStreamHoldStore{rdb: rdb}

	now := time.Now().UTC()
	state := &service.OpenAIStreamHoldState{
		LeaseID:          "lease-1",
		RequestID:        "request-1",
		ClientRequestID:  "client-request-1",
		UserID:           11,
		APIKeyID:         12,
		Platform:         service.PlatformOpenAI,
		Model:            "gpt-5.1",
		RequestPath:      "/v1/responses",
		Phase:            service.OpenAIStreamHoldPhaseHolding,
		Reason:           "upstream_unavailable",
		HoldCycle:        1,
		RequestStartedAt: now.Add(-time.Second),
		HeldSince:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, store.Upsert(ctx, state, 3*time.Second))

	holds, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, holds, 1)
	require.Equal(t, state.LeaseID, holds[0].LeaseID)
	require.Equal(t, state.RequestID, holds[0].RequestID)
	require.Equal(t, state.Phase, holds[0].Phase)

	state.Phase = service.OpenAIStreamHoldPhaseRetrying
	state.HoldCycle = 2
	state.UpdatedAt = now.Add(time.Second)
	require.NoError(t, store.Upsert(ctx, state, 3*time.Second))
	holds, err = store.List(ctx)
	require.NoError(t, err)
	require.Len(t, holds, 1)
	require.Equal(t, service.OpenAIStreamHoldPhaseRetrying, holds[0].Phase)
	require.Equal(t, 2, holds[0].HoldCycle)

	require.NoError(t, store.Delete(ctx, state.LeaseID))
	holds, err = store.List(ctx)
	require.NoError(t, err)
	require.Empty(t, holds)

	require.NoError(t, store.Upsert(ctx, state, 3*time.Second))
	require.NoError(t, rdb.ZAdd(ctx, openAIStreamHoldExpiryKey, redis.Z{
		Score:  0,
		Member: state.LeaseID,
	}).Err())
	holds, err = store.List(ctx)
	require.NoError(t, err)
	require.Empty(t, holds)
	require.False(t, mr.Exists(openAIStreamHoldStateKey))
	require.False(t, mr.Exists(openAIStreamHoldExpiryKey))
}

func TestOpenAIStreamHoldStoreSeparatesConcurrentDuplicateRequestIDs(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := &openAIStreamHoldStore{rdb: rdb}
	now := time.Now().UTC()

	for _, leaseID := range []string{"lease-1", "lease-2"} {
		require.NoError(t, store.Upsert(ctx, &service.OpenAIStreamHoldState{
			LeaseID:          leaseID,
			RequestID:        "shared-request-id",
			ClientRequestID:  leaseID,
			Phase:            service.OpenAIStreamHoldPhaseHolding,
			Reason:           "upstream_unavailable",
			RequestStartedAt: now,
			HeldSince:        now,
			UpdatedAt:        now,
		}, 3*time.Second))
	}

	holds, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, holds, 2)
	leaseIDs := []string{holds[0].LeaseID, holds[1].LeaseID}
	slices.Sort(leaseIDs)
	require.Equal(t, []string{"lease-1", "lease-2"}, leaseIDs)
	require.Equal(t, "shared-request-id", holds[0].RequestID)
	require.Equal(t, "shared-request-id", holds[1].RequestID)
}
