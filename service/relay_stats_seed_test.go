package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require" // used in TestInjectSeedSummaries
)

// ---------------------------------------------------------------------------
// Unit tests for SeedFromLogs helpers (no DB needed)
// ---------------------------------------------------------------------------

// TestSeedWindowSummaryFields verifies that a manually constructed seed
// WindowSummary has the Seeded flag set and that ComputeChannelScore produces
// a plausible result.
func TestSeedWindowSummaryFields(t *testing.T) {
	t.Parallel()

	now := time.Now()
	since := now.Add(-30 * 24 * time.Hour)

	avgDurationMs := 1200.0 // 1.2 s
	totalCount := int64(100)
	totalDurationNs := int64(avgDurationMs * 1e6 * float64(totalCount))

	s := WindowSummary{
		WindowStart:     since,
		WindowEnd:       now,
		ModelName:       "gpt-4o",
		ChannelID:       7,
		TotalAttempts:   totalCount,
		SuccessAttempts: 95,
		FailedAttempts:  5,
		TotalDurationNs: totalDurationNs,
		AvgDurationMs:   avgDurationMs,
		Seeded:          true,
	}
	s.ChannelScore = ComputeChannelScore(s)

	assert.True(t, s.Seeded, "Seeded flag should be set")
	assert.Equal(t, "gpt-4o", s.ModelName)
	assert.Equal(t, 7, s.ChannelID)
	assert.Equal(t, int64(100), s.TotalAttempts)
	assert.Equal(t, int64(95), s.SuccessAttempts)
	assert.Equal(t, int64(5), s.FailedAttempts)
	assert.InDelta(t, 1200.0, s.AvgDurationMs, 0.001)
	assert.Greater(t, s.ChannelScore, 0.0)
	assert.LessOrEqual(t, s.ChannelScore, 100.0)
}

// TestSeedWindowSummaryScore_HighSuccess checks that a high-success summary
// scores better than a low-success summary.
func TestSeedWindowSummaryScore_HighSuccess(t *testing.T) {
	t.Parallel()

	now := time.Now()
	since := now.Add(-30 * 24 * time.Hour)

	makeSeeded := func(total, success int64, avgMs float64) WindowSummary {
		s := WindowSummary{
			WindowStart:     since,
			WindowEnd:       now,
			ModelName:       "test-model",
			ChannelID:       1,
			TotalAttempts:   total,
			SuccessAttempts: success,
			FailedAttempts:  total - success,
			AvgDurationMs:   avgMs,
			TotalDurationNs: int64(avgMs * 1e6 * float64(total)),
			Seeded:          true,
		}
		s.ChannelScore = ComputeChannelScore(s)
		return s
	}

	highSuccess := makeSeeded(100, 98, 800)
	lowSuccess := makeSeeded(100, 40, 5000)

	assert.Greater(t, highSuccess.ChannelScore, lowSuccess.ChannelScore,
		"high-success channel should score higher than low-success channel")
}

// TestSeedFromLogs_NilDB verifies that SeedFromLogs returns nil (not an error)
// when no DB is configured, so startup does not fail on test/noop setups.
func TestSeedFromLogs_NilDB(t *testing.T) {
	t.Parallel()

	// model.LOG_DB is nil in unit tests (no DB initialised).
	seeds, err := SeedFromLogs(30, nil)
	assert.NoError(t, err)
	assert.Nil(t, seeds)
}

// TestSeedFromLogs_ZeroLookback uses the default when lookbackDays <= 0.
func TestSeedFromLogs_ZeroLookback(t *testing.T) {
	t.Parallel()

	// model.LOG_DB is nil; just ensure we don't panic and return gracefully.
	seeds, err := SeedFromLogs(0, nil)
	assert.NoError(t, err)
	assert.Nil(t, seeds)
}

// TestInjectSeedSummaries verifies that InjectSeedSummaries pushes summaries
// into the ring buffer and they are retrievable via GetWindowSummaries.
func TestInjectSeedSummaries(t *testing.T) {
	t.Parallel()

	classifier := NewRuleBasedClassifier(nil)
	collector := NewMemoryStatsCollector(classifier, 5*time.Minute, 1000)
	defer collector.windowBuf.Stop()

	now := time.Now()
	seeds := []WindowSummary{
		{
			WindowStart:     now.Add(-30 * 24 * time.Hour),
			WindowEnd:       now,
			ModelName:       "claude-3-opus",
			ChannelID:       3,
			TotalAttempts:   50,
			SuccessAttempts: 48,
			FailedAttempts:  2,
			AvgDurationMs:   900,
			Seeded:          true,
		},
		{
			WindowStart:     now.Add(-30 * 24 * time.Hour),
			WindowEnd:       now,
			ModelName:       "gemini-pro",
			ChannelID:       5,
			TotalAttempts:   120,
			SuccessAttempts: 115,
			FailedAttempts:  5,
			AvgDurationMs:   1100,
			Seeded:          true,
		},
	}
	for i := range seeds {
		seeds[i].ChannelScore = ComputeChannelScore(seeds[i])
	}

	collector.InjectSeedSummaries(seeds)

	got := collector.summaries.Snapshot(0)
	require.Len(t, got, 2, "both seeded summaries should be in the ring buffer")

	for _, s := range got {
		assert.True(t, s.Seeded, "injected summary should have Seeded=true")
		assert.Greater(t, s.ChannelScore, 0.0)
	}
}

// TestInjectSeedSummaries_Empty ensures empty/nil seeds do nothing.
func TestInjectSeedSummaries_Empty(t *testing.T) {
	t.Parallel()

	classifier := NewRuleBasedClassifier(nil)
	collector := NewMemoryStatsCollector(classifier, 5*time.Minute, 1000)
	defer collector.windowBuf.Stop()

	collector.InjectSeedSummaries(nil)
	collector.InjectSeedSummaries([]WindowSummary{})

	got := collector.summaries.Snapshot(0)
	assert.Empty(t, got, "empty inject should not add any summaries")
}

// TestSeedCoveredByExisting verifies that combos already in 'existing' (real
// data) are excluded from seeding, while new combos are seeded.
func TestSeedCoveredByExisting(t *testing.T) {
	t.Parallel()

	// Build a set of "existing" real summaries.
	existing := []WindowSummary{
		{
			ModelName: "gpt-4o",
			ChannelID: 1,
			Seeded:    false, // real data
		},
		{
			ModelName: "claude-3",
			ChannelID: 2,
			Seeded:    false, // real data
		},
	}

	// Simulate what SeedFromLogs does when checking coverage.
	type comboKey struct {
		ChannelID int
		ModelName string
	}
	covered := make(map[comboKey]struct{})
	for _, s := range existing {
		if !s.Seeded {
			covered[comboKey{s.ChannelID, s.ModelName}] = struct{}{}
		}
	}

	candidates := []struct {
		channelID int
		model     string
		wantSeed  bool
	}{
		{1, "gpt-4o", false},    // covered by real data
		{2, "claude-3", false},  // covered by real data
		{3, "gemini-pro", true}, // new combo — should be seeded
		{1, "mistral", true},    // same channel, different model — new
	}

	for _, c := range candidates {
		key := comboKey{c.channelID, c.model}
		_, isCovered := covered[key]
		shouldSeed := !isCovered
		assert.Equal(t, c.wantSeed, shouldSeed,
			"channel=%d model=%s: wantSeed=%v", c.channelID, c.model, c.wantSeed)
	}

	// Also verify that seeded summaries in existing do NOT block re-seeding,
	// because we only add non-seeded entries to covered.
	existingWithSeed := []WindowSummary{
		{ModelName: "gpt-4o", ChannelID: 1, Seeded: true}, // previously seeded — should NOT block
	}
	coveredWithSeed := make(map[comboKey]struct{})
	for _, s := range existingWithSeed {
		if !s.Seeded {
			coveredWithSeed[comboKey{s.ChannelID, s.ModelName}] = struct{}{}
		}
	}
	_, blockedByOldSeed := coveredWithSeed[comboKey{1, "gpt-4o"}]
	assert.False(t, blockedByOldSeed, "a previously-seeded summary should not block re-seeding")
}
