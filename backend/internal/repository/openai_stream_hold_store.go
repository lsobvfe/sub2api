package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	openAIStreamHoldStateKey  = "ops:stream_hold:{active}:states:v1"
	openAIStreamHoldExpiryKey = "ops:stream_hold:{active}:expiry:v1"
)

var openAIStreamHoldUpsertScript = redis.NewScript(`
local time_parts = redis.call("TIME")
local now_ms = (tonumber(time_parts[1]) * 1000) + math.floor(tonumber(time_parts[2]) / 1000)
while true do
  local expired = redis.call("ZRANGEBYSCORE", KEYS[2], "-inf", now_ms, "LIMIT", 0, 500)
  if #expired == 0 then
    break
  end
  redis.call("HDEL", KEYS[1], unpack(expired))
  redis.call("ZREM", KEYS[2], unpack(expired))
end
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("ZADD", KEYS[2], now_ms + tonumber(ARGV[3]), ARGV[1])
redis.call("PEXPIRE", KEYS[1], ARGV[4])
redis.call("PEXPIRE", KEYS[2], ARGV[4])
return 1
`)

var openAIStreamHoldDeleteScript = redis.NewScript(`
redis.call("HDEL", KEYS[1], ARGV[1])
redis.call("ZREM", KEYS[2], ARGV[1])
if redis.call("HLEN", KEYS[1]) == 0 then
  redis.call("DEL", KEYS[1], KEYS[2])
end
return 1
`)

var openAIStreamHoldListScript = redis.NewScript(`
local time_parts = redis.call("TIME")
local now_ms = (tonumber(time_parts[1]) * 1000) + math.floor(tonumber(time_parts[2]) / 1000)
while true do
  local expired = redis.call("ZRANGEBYSCORE", KEYS[2], "-inf", now_ms, "LIMIT", 0, 500)
  if #expired == 0 then
    break
  end
  redis.call("HDEL", KEYS[1], unpack(expired))
  redis.call("ZREM", KEYS[2], unpack(expired))
end
if redis.call("HLEN", KEYS[1]) == 0 then
  redis.call("DEL", KEYS[1], KEYS[2])
  return {}
end
return redis.call("HGETALL", KEYS[1])
`)

type openAIStreamHoldStore struct {
	rdb *redis.Client
}

func NewOpenAIStreamHoldStore(rdb *redis.Client) service.OpenAIStreamHoldStore {
	return &openAIStreamHoldStore{rdb: rdb}
}

func (s *openAIStreamHoldStore) Upsert(
	ctx context.Context,
	state *service.OpenAIStreamHoldState,
	leaseTTL time.Duration,
) error {
	if s == nil || s.rdb == nil {
		return service.ErrOpenAIStreamHoldTrackerUnavailable
	}
	if state == nil {
		return errors.New("stream hold state is required")
	}
	if state.LeaseID == "" {
		return errors.New("stream hold lease_id is required")
	}
	if leaseTTL <= 0 {
		return errors.New("stream hold lease TTL must be positive")
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal OpenAI stream hold state: %w", err)
	}
	keyTTL := leaseTTL * 2
	return openAIStreamHoldUpsertScript.Run(
		ctx,
		s.rdb,
		[]string{openAIStreamHoldStateKey, openAIStreamHoldExpiryKey},
		state.LeaseID,
		payload,
		leaseTTL.Milliseconds(),
		keyTTL.Milliseconds(),
	).Err()
}

func (s *openAIStreamHoldStore) Delete(ctx context.Context, leaseID string) error {
	if s == nil || s.rdb == nil {
		return service.ErrOpenAIStreamHoldTrackerUnavailable
	}
	return openAIStreamHoldDeleteScript.Run(
		ctx,
		s.rdb,
		[]string{openAIStreamHoldStateKey, openAIStreamHoldExpiryKey},
		leaseID,
	).Err()
}

func (s *openAIStreamHoldStore) List(ctx context.Context) ([]*service.OpenAIStreamHoldState, error) {
	if s == nil || s.rdb == nil {
		return nil, service.ErrOpenAIStreamHoldTrackerUnavailable
	}
	raw, err := openAIStreamHoldListScript.Run(
		ctx,
		s.rdb,
		[]string{openAIStreamHoldStateKey, openAIStreamHoldExpiryKey},
	).StringSlice()
	if err != nil {
		return nil, err
	}
	if len(raw)%2 != 0 {
		return nil, errors.New("invalid stream hold Redis hash response")
	}

	holds := make([]*service.OpenAIStreamHoldState, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		var state service.OpenAIStreamHoldState
		if err := json.Unmarshal([]byte(raw[i+1]), &state); err != nil {
			return nil, fmt.Errorf("decode OpenAI stream hold %q: %w", raw[i], err)
		}
		state.LeaseID = raw[i]
		holds = append(holds, &state)
	}
	return holds, nil
}

var _ service.OpenAIStreamHoldStore = (*openAIStreamHoldStore)(nil)
