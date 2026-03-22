package service

import (
	"sync"
	"time"
)

const defaultWindowDuration = 5 * time.Minute

// WindowSummary is the aggregated output of one time window, keyed by model+channel+group.
type WindowSummary struct {
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`

	// Dimension keys
	ModelName string `json:"model_name"`
	ChannelID int    `json:"channel_id,omitempty"`
	Group     string `json:"group,omitempty"`

	// Attempt-level (sync + async submit)
	TotalAttempts        int64      `json:"total_attempts"`
	SuccessAttempts      int64      `json:"success_attempts"`
	FailedAttempts       int64      `json:"failed_attempts"`
	ExcludedAttempts     int64      `json:"excluded_attempts"`
	ErrorLevelDist       [4]int64   `json:"error_level_dist"`
	TPS                  float64    `json:"tps"`
	AvgDurationMs        float64    `json:"avg_duration_ms"`
	AvgFirstTokenMs      float64    `json:"avg_first_token_ms"`
	TotalDurationNs      int64      `json:"-"`
	TotalFirstTokenNs    int64      `json:"-"`
	FirstTokenCount      int64      `json:"-"`

	// Request-level (final outcome)
	TotalRequests   int64   `json:"total_requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailedRequests  int64   `json:"failed_requests"`
	RetryRequests   int64   `json:"retry_requests"`
	RetryRecovered  int64   `json:"retry_recovered"`
	RecoveryRate    float64 `json:"recovery_rate"`

	// Async task execution
	TaskExecCount     int64   `json:"task_exec_count"`
	TaskExecSuccess   int64   `json:"task_exec_success"`
	TaskExecDurationNs int64  `json:"-"`
	AvgExecDurationMs float64 `json:"avg_exec_duration_ms"`

	// Channel health score (0-100)
	ChannelScore float64 `json:"channel_score"`
}

// dimKey produces a string key combining the three dimension fields.
func (s *WindowSummary) dimKey() string {
	return s.ModelName + "|" + string(rune(s.ChannelID)) + "|" + s.Group
}

// windowBucketKey is a composite key used to index mutable buckets during collection.
type windowBucketKey struct {
	ModelName string
	ChannelID int
	Group     string
}

// windowBucket accumulates raw counters during one time window.
type windowBucket struct {
	totalAttempts     int64
	successAttempts   int64
	failedAttempts    int64
	excludedAttempts  int64
	errorLevelDist    [4]int64
	totalDurationNs   int64
	totalFirstTokenNs int64
	firstTokenCount   int64

	totalRequests   int64
	successRequests int64
	failedRequests  int64
	retryRequests   int64
	retryRecovered  int64

	taskExecCount      int64
	taskExecSuccess    int64
	taskExecDurationNs int64
}

// WindowBuffer collects raw events within a fixed time window and flushes
// aggregated WindowSummary records when the window expires.
type WindowBuffer struct {
	mu             sync.Mutex
	windowDuration time.Duration
	windowStart    time.Time
	buckets        map[windowBucketKey]*windowBucket
	classifier     ErrorClassifier
	onFlush        func(summaries []WindowSummary) // called after each flush
	stopCh         chan struct{}
}

func NewWindowBuffer(classifier ErrorClassifier, windowDuration time.Duration, onFlush func([]WindowSummary)) *WindowBuffer {
	if windowDuration <= 0 {
		windowDuration = defaultWindowDuration
	}
	wb := &WindowBuffer{
		windowDuration: windowDuration,
		windowStart:    time.Now().Truncate(windowDuration),
		buckets:        make(map[windowBucketKey]*windowBucket),
		classifier:     classifier,
		onFlush:        onFlush,
		stopCh:         make(chan struct{}),
	}
	go wb.flushLoop()
	return wb
}

func (wb *WindowBuffer) Stop() {
	close(wb.stopCh)
}

func (wb *WindowBuffer) flushLoop() {
	for {
		now := time.Now()
		nextFlush := wb.windowStart.Add(wb.windowDuration)
		sleepDuration := nextFlush.Sub(now)
		if sleepDuration <= 0 {
			sleepDuration = time.Millisecond
		}
		select {
		case <-time.After(sleepDuration):
			summaries := wb.Flush()
			if wb.onFlush != nil && len(summaries) > 0 {
				wb.onFlush(summaries)
			}
		case <-wb.stopCh:
			return
		}
	}
}

func (wb *WindowBuffer) getBucket(key windowBucketKey) *windowBucket {
	b, ok := wb.buckets[key]
	if !ok {
		b = &windowBucket{}
		wb.buckets[key] = b
	}
	return b
}

// CollectAttempt adds an attempt event to the current window.
// It also performs error classification and sets Excluded/ErrorLevel on the event.
func (wb *WindowBuffer) CollectAttempt(event *AttemptEvent) {
	event.Timestamp = time.Now()

	if !event.Success && wb.classifier != nil {
		excluded, level, reason := wb.classifier.Classify(*event)
		event.Excluded = excluded
		event.ExcludeReason = reason
		event.ErrorLevel = level
	}

	wb.mu.Lock()
	defer wb.mu.Unlock()

	key := windowBucketKey{ModelName: event.ModelName, ChannelID: event.ChannelID, Group: event.Group}
	b := wb.getBucket(key)

	b.totalAttempts++
	if event.Success {
		b.successAttempts++
	} else if event.Excluded {
		b.excludedAttempts++
		b.errorLevelDist[0]++
	} else {
		b.failedAttempts++
		lvl := event.ErrorLevel
		if lvl < 0 {
			lvl = 1
		}
		if lvl > 3 {
			lvl = 3
		}
		b.errorLevelDist[lvl]++
	}

	b.totalDurationNs += int64(event.Duration)
	if event.FirstTokenDuration > 0 {
		b.totalFirstTokenNs += int64(event.FirstTokenDuration)
		b.firstTokenCount++
	}
}

func (wb *WindowBuffer) CollectRequestComplete(event *RequestCompleteEvent) {
	event.Timestamp = time.Now()

	wb.mu.Lock()
	defer wb.mu.Unlock()

	key := windowBucketKey{ModelName: event.OriginalModel, Group: event.Group}
	b := wb.getBucket(key)

	b.totalRequests++
	if event.FinalSuccess {
		b.successRequests++
	} else {
		b.failedRequests++
	}
	if event.HasRetry {
		b.retryRequests++
	}
	if event.RetryRecovered {
		b.retryRecovered++
	}
}

func (wb *WindowBuffer) CollectTaskExecution(event *TaskExecutionEvent) {
	event.Timestamp = time.Now()

	if !event.Success && event.FailReason != "" && wb.classifier != nil {
		excluded, level, reason := wb.classifier.ClassifyTaskFailReason(event.ModelName, event.FailReason)
		event.Excluded = excluded
		event.ExcludeReason = reason
		event.ErrorLevel = level
	}

	wb.mu.Lock()
	defer wb.mu.Unlock()

	key := windowBucketKey{ModelName: event.ModelName, ChannelID: event.ChannelID, Group: event.Group}
	b := wb.getBucket(key)

	b.taskExecCount++
	if event.Success {
		b.taskExecSuccess++
	}
	b.taskExecDurationNs += int64(event.ExecutionDuration)
}

func buildSummaries(buckets map[windowBucketKey]*windowBucket, windowStart, windowEnd time.Time, windowSecs float64) []WindowSummary {
	if len(buckets) == 0 {
		return nil
	}
	summaries := make([]WindowSummary, 0, len(buckets))
	for key, b := range buckets {
		s := WindowSummary{
			WindowStart:       windowStart,
			WindowEnd:         windowEnd,
			ModelName:         key.ModelName,
			ChannelID:         key.ChannelID,
			Group:             key.Group,
			TotalAttempts:     b.totalAttempts,
			SuccessAttempts:   b.successAttempts,
			FailedAttempts:    b.failedAttempts,
			ExcludedAttempts:  b.excludedAttempts,
			ErrorLevelDist:    b.errorLevelDist,
			TotalDurationNs:   b.totalDurationNs,
			TotalFirstTokenNs: b.totalFirstTokenNs,
			FirstTokenCount:   b.firstTokenCount,
			TotalRequests:     b.totalRequests,
			SuccessRequests:   b.successRequests,
			FailedRequests:    b.failedRequests,
			RetryRequests:     b.retryRequests,
			RetryRecovered:    b.retryRecovered,
			TaskExecCount:     b.taskExecCount,
			TaskExecSuccess:   b.taskExecSuccess,
			TaskExecDurationNs: b.taskExecDurationNs,
		}
		if b.totalAttempts > 0 {
			s.TPS = float64(b.totalAttempts) / windowSecs
			s.AvgDurationMs = float64(b.totalDurationNs) / float64(b.totalAttempts) / 1e6
		}
		if b.firstTokenCount > 0 {
			s.AvgFirstTokenMs = float64(b.totalFirstTokenNs) / float64(b.firstTokenCount) / 1e6
		}
		if b.retryRequests > 0 {
			s.RecoveryRate = float64(b.retryRecovered) / float64(b.retryRequests)
		}
		if b.taskExecCount > 0 {
			s.AvgExecDurationMs = float64(b.taskExecDurationNs) / float64(b.taskExecCount) / 1e6
		}
		s.ChannelScore = ComputeChannelScore(s)
		summaries = append(summaries, s)
	}
	return summaries
}

// Flush computes WindowSummary for all buckets and resets the buffer.
func (wb *WindowBuffer) Flush() []WindowSummary {
	wb.mu.Lock()
	buckets := wb.buckets
	windowStart := wb.windowStart
	windowEnd := windowStart.Add(wb.windowDuration)
	wb.buckets = make(map[windowBucketKey]*windowBucket)
	wb.windowStart = time.Now().Truncate(wb.windowDuration)
	wb.mu.Unlock()

	return buildSummaries(buckets, windowStart, windowEnd, wb.windowDuration.Seconds())
}

// Peek returns a snapshot of the current (unflushed) window as summaries,
// without resetting the buffer. Used to include real-time data in queries.
func (wb *WindowBuffer) Peek() []WindowSummary {
	wb.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(wb.windowStart).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	snapshot := make(map[windowBucketKey]*windowBucket, len(wb.buckets))
	for k, b := range wb.buckets {
		copy := *b
		snapshot[k] = &copy
	}
	windowStart := wb.windowStart
	wb.mu.Unlock()

	return buildSummaries(snapshot, windowStart, now, elapsed)
}
