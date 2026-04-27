package service

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// DefaultSeedLookbackDays is the number of days of log history used when
// seeding channel scores at startup.
const DefaultSeedLookbackDays = 30

// logSeedRow holds one row returned by the aggregation query.
type logSeedRow struct {
	ChannelID    int
	ModelName    string
	TotalCount   int64
	SuccessCount int64
	ErrorCount   int64
	AvgUseTime   float64 // average use_time in seconds
}

// SeedFromLogs queries the logs table for the last lookbackDays days,
// aggregates per channel_id + model_name, and generates synthetic
// WindowSummary records that can be injected into the ring buffer.
//
// Summaries are only generated for channel+model combos that do NOT already
// appear in existing (i.e. real or previously-persisted summaries).
//
// Pass existing = nil (or an empty slice) to seed all combos unconditionally.
func SeedFromLogs(lookbackDays int, existing []WindowSummary) ([]WindowSummary, error) {
	if lookbackDays <= 0 {
		lookbackDays = DefaultSeedLookbackDays
	}

	db := model.LOG_DB
	if db == nil {
		return nil, nil
	}

	// Build a set of channel+model combos that already have window summaries.
	type comboKey struct {
		ChannelID int
		ModelName string
	}
	covered := make(map[comboKey]struct{}, len(existing))
	for _, s := range existing {
		if !s.Seeded {
			// Only real (non-seeded) summaries block seeding.
			covered[comboKey{s.ChannelID, s.ModelName}] = struct{}{}
		}
	}

	since := time.Now().Add(-time.Duration(lookbackDays) * 24 * time.Hour)
	sinceUnix := since.Unix()

	// Aggregate query — compatible with SQLite, MySQL ≥ 5.7.8, PostgreSQL ≥ 9.6.
	// CASE WHEN is standard SQL; column names are not reserved words.
	const query = `
SELECT
    channel_id,
    model_name,
    COUNT(*)                                      AS total_count,
    SUM(CASE WHEN type = 2 THEN 1 ELSE 0 END)     AS success_count,
    SUM(CASE WHEN type = 5 THEN 1 ELSE 0 END)     AS error_count,
    AVG(CAST(use_time AS FLOAT))                  AS avg_use_time
FROM logs
WHERE created_at >= ?
  AND channel_id > 0
  AND model_name != ''
GROUP BY channel_id, model_name
`
	rows, err := db.Raw(query, sinceUnix).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var seeds []WindowSummary
	windowStart := since
	windowEnd := time.Now()

	for rows.Next() {
		var r logSeedRow
		if err := rows.Scan(&r.ChannelID, &r.ModelName, &r.TotalCount, &r.SuccessCount, &r.ErrorCount, &r.AvgUseTime); err != nil {
			common.SysError("stats seed: scan error: " + err.Error())
			continue
		}
		if r.TotalCount <= 0 {
			continue
		}

		key := comboKey{r.ChannelID, r.ModelName}
		if _, exists := covered[key]; exists {
			// Real data already present — skip seeding this combo.
			continue
		}

		avgDurationMs := r.AvgUseTime * 1000 // seconds → milliseconds

		// Synthetic TotalDurationNs so ComputeChannelScore gets a real AvgDurationMs.
		totalDurationNs := int64(avgDurationMs * 1e6 * float64(r.TotalCount))

		s := WindowSummary{
			WindowStart:     windowStart,
			WindowEnd:       windowEnd,
			ModelName:       r.ModelName,
			ChannelID:       r.ChannelID,
			TotalAttempts:   r.TotalCount,
			SuccessAttempts: r.SuccessCount,
			FailedAttempts:  r.ErrorCount,
			TotalDurationNs: totalDurationNs,
			AvgDurationMs:   avgDurationMs,
			Seeded:          true,
		}

		s.ChannelScore = ComputeChannelScore(s)
		seeds = append(seeds, s)
	}

	if err := rows.Err(); err != nil {
		return seeds, err
	}
	return seeds, nil
}
