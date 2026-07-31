//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type opsStreamHoldTrackerStub struct {
	filter   service.OpenAIStreamHoldFilter
	snapshot *service.OpenAIStreamHoldSnapshot
}

func (s *opsStreamHoldTrackerStub) Upsert(context.Context, *service.OpenAIStreamHoldState) error {
	return nil
}

func (s *opsStreamHoldTrackerStub) Delete(context.Context, string) error {
	return nil
}

func (s *opsStreamHoldTrackerStub) List(
	_ context.Context,
	filter service.OpenAIStreamHoldFilter,
) (*service.OpenAIStreamHoldSnapshot, error) {
	s.filter = filter
	return s.snapshot, nil
}

func (s *opsStreamHoldTrackerStub) HeartbeatInterval() time.Duration {
	return time.Second
}

func (s *opsStreamHoldTrackerStub) OperationTimeout() time.Duration {
	return time.Second
}

func TestGetActiveStreamHoldsAppliesRealtimeFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(12)
	accountID := int64(13)
	tracker := &opsStreamHoldTrackerStub{
		snapshot: &service.OpenAIStreamHoldSnapshot{
			Enabled: true,
			Holds: []*service.OpenAIStreamHoldState{
				{RequestID: "request-hold-1", Phase: service.OpenAIStreamHoldPhaseHolding},
			},
			Summary: service.OpenAIStreamHoldSummary{ActiveCount: 1},
		},
	}
	opsService := service.NewOpsService(
		nil,
		nil,
		&config.Config{Ops: config.OpsConfig{Enabled: true}},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	handler := NewOpsHandler(opsService, tracker)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/ops/stream-holds?platform=openai&group_id=12&account_id=13",
		nil,
	)

	handler.GetActiveStreamHolds(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, service.PlatformOpenAI, tracker.filter.Platform)
	require.Equal(t, groupID, *tracker.filter.GroupID)
	require.Equal(t, accountID, *tracker.filter.AccountID)
	require.Contains(t, recorder.Body.String(), `"request_id":"request-hold-1"`)
}

func TestGetActiveStreamHoldsRejectsInvalidGroupFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewOpsHandler(
		service.NewOpsService(nil, nil, &config.Config{Ops: config.OpsConfig{Enabled: true}}, nil, nil, nil, nil, nil, nil, nil, nil),
		&opsStreamHoldTrackerStub{},
	)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/ops/stream-holds?group_id=invalid", nil)

	handler.GetActiveStreamHolds(c)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}
