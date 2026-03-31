package controller

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

// GetRelayStats returns lifetime aggregated counters.
//
// Request:
//
//	GET /api/relay/stats/
//
// Response:
//
//	{
//	  "success": true,
//	  "data": { StatsCounters fields }
//	}
func GetRelayStats(c *gin.Context) {
	collector := service.GetRelayStatsCollector()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    collector.GetCounters(),
	})
}

// GetRelayStatsWindows returns recent window summaries.
//
// Request:
//
//	GET /api/relay/stats/windows?limit=100
//
// Query params:
//   - limit: max number of window summaries to return (default 100, max 1000)
//
// Response:
//
//	{
//	  "success": true,
//	  "data": [ WindowSummary, ... ]
//	}
func GetRelayStatsWindows(c *gin.Context) {
	collector := service.GetRelayStatsCollector()
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    collector.GetWindowSummaries(limit),
	})
}

// GetRelayStatsTimeSeries returns time-series data for chart rendering.
// Each dimension value becomes a separate series (line) with timestamped points.
//
// Request:
//
//	GET /api/relay/stats/timeseries?group_by=channel&metric=success_rate&interval=1h&range=24h
//
// Query params:
//   - group_by: dimension for grouping — "model", "channel", "group" (default "model")
//   - metric: value to plot — "success_rate", "tps", "avg_duration", "avg_first_token",
//     "channel_score", "request_success_rate", "task_exec_success_rate" (default "success_rate")
//   - interval: aggregation granularity — "5m", "1h", "6h" (default "5m", the raw window size)
//   - range: time range to look back — "1h", "6h", "24h", "7d" (default "24h")
//
// Response:
//
//	{
//	  "success": true,
//	  "data": {
//	    "series": [
//	      {
//	        "key": "channel_1",
//	        "label": "channel_1",
//	        "points": [
//	          { "time": "2026-03-09T08:00:00Z", "value": 99.2 },
//	          { "time": "2026-03-09T09:00:00Z", "value": 98.5 }
//	        ]
//	      }
//	    ],
//	    "metric": "success_rate",
//	    "interval": "1h",
//	    "group_by": "channel"
//	  }
//	}
func GetRelayStatsTimeSeries(c *gin.Context) {
	collector := service.GetRelayStatsCollector()

	groupBy := c.DefaultQuery("group_by", "model")
	metric := c.DefaultQuery("metric", "success_rate")
	intervalStr := c.DefaultQuery("interval", "5m")
	rangeStr := c.DefaultQuery("range", "24h")

	interval := parseDuration(intervalStr, 5*time.Minute)
	timeRange := parseDuration(rangeStr, 24*time.Hour)

	result := collector.GetTimeSeries(service.TimeSeriesQuery{
		GroupBy:  groupBy,
		Metric:   metric,
		Interval: interval,
		Range:    timeRange,
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    result,
	})
}

// GetRelayStatsDimensions aggregates window summaries grouped by dimensions.
//
// Request:
//
//	GET /api/relay/stats/dimensions?group_by=model
//
// Query params:
//   - group_by: comma-separated dimension names — "model", "channel", "group" (default "model")
//
// Response:
//
//	{
//	  "success": true,
//	  "data": { "gpt-4": { StatsCounters }, "claude-3": { StatsCounters } },
//	  "group_by": ["model"]
//	}
func GetRelayStatsDimensions(c *gin.Context) {
	groupBy := c.DefaultQuery("group_by", "model")
	dimensions := strings.Split(groupBy, ",")
	for i := range dimensions {
		dimensions[i] = strings.TrimSpace(dimensions[i])
	}

	collector := service.GetRelayStatsCollector()
	result := collector.AggregateWindows(dimensions)
	if result == nil {
		result = make(map[string]service.StatsCounters)
	}
	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"data":     result,
		"group_by": dimensions,
	})
}

// ResetRelayStats clears all in-memory stats.
//
// Request:
//
//	DELETE /api/relay/stats/reset
//
// Response:
//
//	{ "success": true, "message": "stats reset" }
func ResetRelayStats(c *gin.Context) {
	collector := service.GetRelayStatsCollector()
	collector.Reset()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "stats reset",
	})
}

// GetStatsExclusionRules returns the current error exclusion/classification rules.
//
// Request:
//
//	GET /api/relay/stats/exclusion_rules
//
// Response:
//
//	{
//	  "success": true,
//	  "data": [ ErrorExclusionRule, ... ]
//	}
func GetStatsExclusionRules(c *gin.Context) {
	classifier, ok := service.GetErrorClassifier().(*service.RuleBasedClassifier)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": []any{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    classifier.GetRules(),
	})
}

// UpdateStatsExclusionRules replaces the current error exclusion/classification rules.
//
// Request:
//
//	PUT /api/relay/stats/exclusion_rules
//	Body: [ ErrorExclusionRule, ... ]
//
// Each rule has fields: model, channel_types, error_codes, status_codes,
// message_keywords, level (0=exclude, 1=normal, 2=serious, 3=critical), description.
//
// Response:
//
//	{ "success": true, "message": "rules updated" }
func UpdateStatsExclusionRules(c *gin.Context) {
	var rules []service.ErrorExclusionRule
	if err := c.ShouldBindJSON(&rules); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	classifier, ok := service.GetErrorClassifier().(*service.RuleBasedClassifier)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "classifier not initialized"})
		return
	}
	classifier.UpdateRules(rules)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "rules updated",
	})
}

// GetStatsScoreWeights returns the current channel health scoring weights.
//
// Request:
//
//	GET /api/relay/stats/score_weights
//
// Response:
//
//	{ "success": true, "data": ScoreWeights }
func GetStatsScoreWeights(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    service.GetScoreWeights(),
	})
}

// UpdateStatsScoreWeights replaces the current channel health scoring weights.
//
// Request:
//
//	PUT /api/relay/stats/score_weights
//	Body: ScoreWeights (partial or full; merged with defaults)
//
// Response:
//
//	{ "success": true, "message": "score weights updated" }
func UpdateStatsScoreWeights(c *gin.Context) {
	var w service.ScoreWeights
	if err := c.ShouldBindJSON(&w); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	service.SetScoreWeights(w)
	data, _ := common.Marshal(w)
	operation_setting.StatsScoreWeightsFromString(string(data))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "score weights updated",
	})
}

// GetUserModelStats returns per-model statistics visible to authenticated users.
// No channel IDs, channel names, or channel scores are exposed.
//
// Request:
//
//	GET /api/relay/stats/models?start_timestamp=1773467002&end_timestamp=1773557002
//
// Query params:
//   - start_timestamp: unix timestamp, filter window summaries ending after this time (optional)
//   - end_timestamp: unix timestamp, filter window summaries starting before this time (optional)
//
// Response:
//
//	{
//	  "success": true,
//	  "data": [
//	    {
//	      "model_name": "gpt-4o",
//	      "success_rate": 99.2,
//	      "avg_duration_ms": 1200,
//	      "avg_first_token_ms": 350,
//	      "total_requests": 500,
//	      "success_requests": 496,
//	      "failed_requests": 4
//	    }
//	  ]
//	}
func GetUserModelStats(c *gin.Context) {
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)

	collector := service.GetRelayStatsCollector()
	stats := collector.GetModelStats(startTimestamp, endTimestamp)
	if stats == nil {
		stats = []service.ModelStats{}
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stats,
	})
}

// parseDuration parses a human-friendly duration string like "5m", "1h", "24h", "7d".
func parseDuration(s string, fallback time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
