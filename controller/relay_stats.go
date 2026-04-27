package controller

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

// GetRelayStats returns lifetime aggregated counters across all relay traffic
// since process start (admin overview card on the dashboard).
//
// Request:
//
//	GET /api/relay/stats/
//
// Response data fields (all int64 unless noted):
//
//	Request-level (final outcome after all retries):
//	  total_requests       — total relay requests received
//	  success_requests     — requests whose final outcome was success
//	  failed_requests      — requests whose final outcome was failure
//	  retry_requests       — requests that involved at least one retry
//	  retry_recovered      — retry requests that eventually succeeded
//	  retry_recovery_rate  — retry_recovered / retry_requests (float, 0..1)
//
//	Attempt-level (per upstream call; one request may have N attempts):
//	  total_attempts    — total upstream calls made
//	  success_attempts  — successful upstream calls
//	  failed_attempts   — failed upstream calls (excluding excluded ones)
//	  excluded_attempts — failures classified as excluded (e.g. client 4xx)
//
//	Async task (MJ / Suno style jobs):
//	  task_submit_count    — total async task submissions
//	  task_submit_success  — submissions accepted by upstream
//	  task_exec_count      — tasks observed to terminal state
//	  task_exec_success    — tasks that finished successfully
//
// Note: averaged metrics (tps, avg_duration_ms, avg_first_token_ms,
// avg_output_tps, avg_exec_duration_ms) are not present here because they
// require time-window aggregation. Use GET /api/relay/stats/dimensions
// (group_by=model) or GET /api/relay/stats/windows for those metrics.
func GetRelayStats(c *gin.Context) {
	collector := service.GetRelayStatsCollector()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    collector.GetCounters(),
	})
}

// GetRelayStatsWindows returns the most recent per-bucket window summaries.
// Each window is a 5-minute slot keyed by (model, channel_id, group); a single
// 5-minute period therefore produces multiple entries (one per combination).
// Used by the admin "recent windows" table.
//
// Request:
//
//	GET /api/relay/stats/windows?limit=100
//
// Query params:
//   - limit: max number of window summaries to return (default 100, max 1000)
//
// Response data — array of WindowSummary objects:
//
//	window_start / window_end — UTC time bounds of the 5-minute bucket
//	model_name / channel_id / group — dimension keys identifying the bucket
//	seeded                  — true if this row was synthesized from historical
//	                          logs at startup (cold-start), false for real traffic
//
//	Attempt counters (all int64):
//	  total_attempts / async_attempts / success_attempts /
//	  failed_attempts / excluded_attempts
//	  error_level_dist — [excluded, normal, serious, critical] distribution
//
//	Computed metrics (float, within this window only):
//	  tps                 — attempts per second
//	  avg_duration_ms     — mean per-attempt duration
//	  avg_first_token_ms  — mean TTFT (streaming)
//	  avg_output_tps      — completion tokens / second
//	  avg_exec_duration_ms — async task mean execution time
//	  channel_score       — 0..100 health score for the (model, channel, group)
//
//	Request-level:
//	  total_requests / success_requests / failed_requests /
//	  retry_requests / retry_recovered / recovery_rate
//
//	Async task:
//	  task_exec_count / task_exec_success
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
// Each dimension value (e.g. each channel) becomes a separate series (line)
// with timestamped points. Used by the admin dashboard trend charts.
//
// Request:
//
//	GET /api/relay/stats/timeseries?group_by=channel&metric=success_rate&interval=1h&range=24h
//
// Query params:
//   - group_by: dimension used to split into multiple lines
//   - "model"   — one series per model_name
//   - "channel" — one series per channel_id
//   - "group"   — one series per user group (default "model")
//   - metric: which value to plot on the Y axis
//   - "success_rate"            — per-attempt success % (excluding excluded errors)
//   - "tps"                     — attempts / second
//   - "avg_output_tps"          — completion tokens / second
//   - "avg_duration"            — mean per-attempt duration (ms)
//   - "avg_first_token"         — mean TTFT for streaming (ms)
//   - "channel_score"           — 0..100 health score
//   - "request_success_rate"    — final request success % (after retries)
//   - "task_exec_success_rate"  — async task success %
//     (default "success_rate")
//   - interval: slot size to aggregate raw 5-min windows into
//   - "5m" | "1h" | "6h" (default "5m" — one point per raw window)
//   - range: look-back horizon ending at now
//   - "1h" | "6h" | "24h" | "7d" (default "24h")
//
// Response data fields:
//
//	series    — array of lines; each line has:
//	              key    — dimension value used internally (e.g. "1" for channel_id=1)
//	              label  — display label (same as key today; kept for future i18n)
//	              points — array of { time: ISO8601, value: float } entries
//	metric    — echoes the requested metric
//	interval  — echoes the normalized interval ("1h" etc.)
//	group_by  — echoes the requested dimension
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
//
// Each StatsCounters value reuses the same fields as GET /api/relay/stats/:
// total/success/failed requests, total/success/failed/excluded attempts,
// retry metrics, TPS, avg_duration_ms, avg_output_tps, avg_first_token_ms
// (streaming-only mean TTFT), and async task metrics.
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
// Each rule can match by model, channel_types, error_codes, status_codes,
// and/or message_keywords. level meanings:
//   - 0 = excluded from failure stats
//   - 1 = normal failure
//   - 2 = serious failure
//   - 3 = critical failure
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
// message_keywords, level (0=exclude, 1=normal, 2=serious, 3=critical),
// and description.
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
	data, _ := common.Marshal(rules)
	rulesJSON := string(data)
	operation_setting.StatsErrorExclusionRulesFromString(rulesJSON)
	if err := model.UpdateOption("StatsErrorExclusionRules", rulesJSON); err != nil {
		common.SysError("stats: failed to persist exclusion rules: " + err.Error())
	}
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
// Response data fields:
//   - sync: weights and thresholds for synchronous relay scoring
//   - async: weights and thresholds for async task scoring
//   - level_weights: per-error-level severity multipliers [L0, L1, L2, L3]
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
// Common sync fields:
// base_weight, severity_max, recovery_weight, speed_weight, tps_weight,
// baseline_score, sparse_threshold, speed_thresholds, tps_thresholds.
//
// Common async fields:
// submit_base_weight, severity_max, recovery_weight, submit_speed_weight,
// exec_success_weight, exec_speed_weight, baseline_score, sparse_threshold,
// submit_speed_thresholds, exec_speed_thresholds.
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
	weightsJSON := string(data)
	operation_setting.StatsScoreWeightsFromString(weightsJSON)
	if err := model.UpdateOption("StatsScoreWeights", weightsJSON); err != nil {
		common.SysError("stats: failed to persist score weights: " + err.Error())
	}
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
//	      "p50_first_token_ms": 350,   // median TTFT; omitted for non-streaming models
//	      "tps": 45.2,
//	      "total_requests": 500,
//	      "success_requests": 496,
//	      "failed_requests": 4,
//	      "has_data": true
//	    }
//	  ]
//	}
//
// Models that are enabled but have no traffic data are included with
// success_rate=100 (optimistic default) and null numeric metrics so the
// frontend can display them with a "no data yet" indicator.
//
// TTFT note:
//   - p50_first_token_ms is only produced for streaming requests
//   - non-streaming models keep it nil / omitted
//   - live in-memory windows use median (P50); DB-restored historical windows
//     currently fall back to mean TTFT because raw samples are not persisted
func GetUserModelStats(c *gin.Context) {
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)

	collector := service.GetRelayStatsCollector()
	stats := collector.GetModelStats(startTimestamp, endTimestamp)
	if stats == nil {
		stats = []service.ModelStats{}
	}

	visibleModels, err := resolveVisibleModelNames(c)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "get user group failed",
		})
		return
	}
	stats = appendZeroTrafficModelStats(stats, visibleModels)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stats,
	})
}

func appendZeroTrafficModelStats(stats []service.ModelStats, visibleModels []string) []service.ModelStats {
	statsSet := make(map[string]struct{}, len(stats))
	for _, s := range stats {
		statsSet[s.ModelName] = struct{}{}
	}
	for _, modelName := range visibleModels {
		if _, exists := statsSet[modelName]; exists {
			continue
		}
		stats = append(stats, service.ModelStats{
			ModelName:   modelName,
			SuccessRate: 100,
			HasData:     false,
			// AvgDurationMs, P50FirstTokenMs, TPS remain nil → JSON null
		})
	}
	return stats
}

// parseDuration parses a human-friendly duration string like "5m", "1h", "24h", "7d".
func parseDuration(s string, fallback time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if body, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(body)
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
