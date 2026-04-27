package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type stubRelayStatsCollector struct {
	modelStats []service.ModelStats
}

func (s *stubRelayStatsCollector) CollectAttempt(_ *service.AttemptEvent)                {}
func (s *stubRelayStatsCollector) CollectRequestComplete(_ service.RequestCompleteEvent) {}
func (s *stubRelayStatsCollector) CollectTaskExecution(_ service.TaskExecutionEvent)     {}
func (s *stubRelayStatsCollector) GetCounters() service.StatsCounters                    { return service.StatsCounters{} }
func (s *stubRelayStatsCollector) GetWindowSummaries(_ int) []service.WindowSummary      { return nil }
func (s *stubRelayStatsCollector) GetTimeSeries(_ service.TimeSeriesQuery) service.TimeSeriesResult {
	return service.TimeSeriesResult{}
}
func (s *stubRelayStatsCollector) AggregateWindows(_ []string) map[string]service.StatsCounters {
	return nil
}
func (s *stubRelayStatsCollector) GetModelStats(_, _ int64) []service.ModelStats { return s.modelStats }
func (s *stubRelayStatsCollector) Reset()                                        {}

type relayStatsAPIResponse struct {
	Success bool                 `json:"success"`
	Message string               `json:"message"`
	Data    []service.ModelStats `json:"data"`
}

func TestAppendZeroTrafficModelStats_OnlyAddsVisibleModels(t *testing.T) {
	stats := []service.ModelStats{
		{ModelName: "existing-model", SuccessRate: 95, HasData: true},
	}

	got := appendZeroTrafficModelStats(stats, []string{"existing-model", "visible-model"})

	if len(got) != 2 {
		t.Fatalf("expected 2 model stats entries, got %d", len(got))
	}
	if got[1].ModelName != "visible-model" {
		t.Fatalf("expected scoped visible model to be appended, got %q", got[1].ModelName)
	}
	if got[1].HasData {
		t.Fatalf("expected zero-traffic model to have HasData=false")
	}
}

func TestGetUserModelStats_UsesScopedVisibleModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	origCollector := service.GetRelayStatsCollector()
	service.SetRelayStatsCollector(&stubRelayStatsCollector{})
	defer service.SetRelayStatsCollector(origCollector)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/relay/stats/models", nil)
	ctx.Set("id", 1)
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimit, map[string]bool{
		"scoped-model": true,
	})

	GetUserModelStats(ctx)

	var response relayStatsAPIResponse
	if err := common.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}
	if len(response.Data) != 1 {
		t.Fatalf("expected exactly one visible model, got %d", len(response.Data))
	}
	if response.Data[0].ModelName != "scoped-model" {
		t.Fatalf("expected scoped model only, got %q", response.Data[0].ModelName)
	}
}
