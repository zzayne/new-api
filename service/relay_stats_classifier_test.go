package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifier_SuccessEvent_NeverExcluded(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}},
	})

	excluded, level, reason := c.Classify(AttemptEvent{
		Success:    true,
		StatusCode: 400,
	})
	assert.False(t, excluded)
	assert.Equal(t, 0, level)
	assert.Empty(t, reason)
}

func TestClassifier_NoRules(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier(nil)

	excluded, level, _ := c.Classify(AttemptEvent{
		Success:    false,
		StatusCode: 500,
	})
	assert.False(t, excluded)
	assert.Equal(t, 1, level)
}

func TestClassifier_MatchByStatusCode(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400, 422}, Description: "client_error"},
	})

	tests := []struct {
		name       string
		statusCode int
		wantExcl   bool
	}{
		{"match 400", 400, true},
		{"match 422", 422, true},
		{"no match 500", 500, false},
		{"no match 401", 401, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			excl, _, reason := c.Classify(AttemptEvent{
				Success:    false,
				StatusCode: tc.statusCode,
			})
			assert.Equal(t, tc.wantExcl, excl)
			if tc.wantExcl {
				assert.Equal(t, "client_error", reason)
			}
		})
	}
}

func TestClassifier_MatchByErrorCode(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{ErrorCodes: []string{"invalid_request", "context_length_exceeded"}, Description: "param_error"},
	})

	tests := []struct {
		name      string
		errorCode string
		wantExcl  bool
	}{
		{"exact match", "invalid_request", true},
		{"case insensitive", "Invalid_Request", true},
		{"another match", "context_length_exceeded", true},
		{"no match", "internal_error", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			excl, _, _ := c.Classify(AttemptEvent{
				Success:   false,
				ErrorCode: tc.errorCode,
			})
			assert.Equal(t, tc.wantExcl, excl)
		})
	}
}

func TestClassifier_MatchByMessageKeyword(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{MessageKeywords: []string{"context_length_exceeded", "safety block"}, Description: "keyword_match"},
	})

	tests := []struct {
		name     string
		message  string
		wantExcl bool
	}{
		{"keyword in message", "Error: context_length_exceeded for model gpt-4", true},
		{"case insensitive", "SAFETY BLOCK triggered", true},
		{"partial match", "this has context_length_exceeded in it", true},
		{"no match", "internal server error", false},
		{"empty message", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			excl, _, _ := c.Classify(AttemptEvent{
				Success:      false,
				ErrorMessage: tc.message,
			})
			assert.Equal(t, tc.wantExcl, excl)
		})
	}
}

func TestClassifier_ChannelTypeFilter(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{
			ChannelTypes: []int{1, 6},
			StatusCodes:  []int{400},
			Description:  "openai_client_error",
		},
	})

	tests := []struct {
		name        string
		channelType int
		statusCode  int
		wantExcl    bool
	}{
		{"OpenAI + 400", 1, 400, true},
		{"OpenAIMax + 400", 6, 400, true},
		{"Gemini + 400", 24, 400, false},
		{"OpenAI + 500", 1, 500, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			excl, _, _ := c.Classify(AttemptEvent{
				Success:     false,
				ChannelType: tc.channelType,
				StatusCode:  tc.statusCode,
			})
			assert.Equal(t, tc.wantExcl, excl)
		})
	}
}

func TestClassifier_ChannelTypeOnly_NoErrorConditions(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{ChannelTypes: []int{99}, Description: "all_errors_for_channel_99"},
	})

	excl, _, _ := c.Classify(AttemptEvent{
		Success:     false,
		ChannelType: 99,
		StatusCode:  500,
	})
	assert.True(t, excl, "rule with only ChannelTypes should match all errors for that channel")

	excl2, _, _ := c.Classify(AttemptEvent{
		Success:     false,
		ChannelType: 1,
		StatusCode:  500,
	})
	assert.False(t, excl2, "should not match other channel types")
}

func TestClassifier_EmptyRule_NoMatch(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{Description: "empty rule"},
	})

	excl, _, _ := c.Classify(AttemptEvent{
		Success:    false,
		StatusCode: 500,
	})
	assert.False(t, excl, "rule with no conditions and no channel types should never match")
}

func TestClassifier_MultipleRules_OR(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}, Description: "rule1"},
		{ErrorCodes: []string{"rate_limited"}, Description: "rule2"},
		{ChannelTypes: []int{24}, MessageKeywords: []string{"safety"}, Description: "rule3"},
	})

	tests := []struct {
		name       string
		event      AttemptEvent
		wantExcl   bool
		wantReason string
	}{
		{"matches rule1", AttemptEvent{Success: false, StatusCode: 400}, true, "rule1"},
		{"matches rule2", AttemptEvent{Success: false, ErrorCode: "rate_limited", StatusCode: 429}, true, "rule2"},
		{"matches rule3", AttemptEvent{Success: false, ChannelType: 24, ErrorMessage: "content safety filter triggered"}, true, "rule3"},
		{"no match", AttemptEvent{Success: false, StatusCode: 500, ChannelType: 1, ErrorCode: "internal"}, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			excl, _, reason := c.Classify(tc.event)
			assert.Equal(t, tc.wantExcl, excl)
			if tc.wantExcl {
				assert.Equal(t, tc.wantReason, reason)
			}
		})
	}
}

func TestClassifier_ORWithinRule(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{
			ErrorCodes:      []string{"invalid_param"},
			StatusCodes:     []int{422},
			MessageKeywords: []string{"validation failed"},
			Description:     "param_validation",
		},
	})

	tests := []struct {
		name  string
		event AttemptEvent
	}{
		{"by error_code", AttemptEvent{Success: false, ErrorCode: "invalid_param"}},
		{"by status_code", AttemptEvent{Success: false, StatusCode: 422}},
		{"by keyword", AttemptEvent{Success: false, ErrorMessage: "request validation failed: field X"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			excl, _, _ := c.Classify(tc.event)
			assert.True(t, excl)
		})
	}
}

func TestClassifier_UpdateRules(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}},
	})

	excl, _, _ := c.Classify(AttemptEvent{Success: false, StatusCode: 400})
	require.True(t, excl)

	c.UpdateRules([]ErrorExclusionRule{
		{StatusCodes: []int{429}, Description: "rate_limit_only"},
	})

	excl2, _, _ := c.Classify(AttemptEvent{Success: false, StatusCode: 400})
	assert.False(t, excl2, "old rule should no longer match")

	excl3, _, reason := c.Classify(AttemptEvent{Success: false, StatusCode: 429})
	assert.True(t, excl3)
	assert.Equal(t, "rate_limit_only", reason)
}

func TestClassifier_UpdateRules_ClearAll(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}},
	})

	c.UpdateRules(nil)

	excl, _, _ := c.Classify(AttemptEvent{Success: false, StatusCode: 400})
	assert.False(t, excl, "no rules means nothing is excluded")
}

func TestClassifier_GetRules_ReturnsCopy(t *testing.T) {
	t.Parallel()
	original := []ErrorExclusionRule{
		{StatusCodes: []int{400}, Description: "test"},
	}
	c := NewRuleBasedClassifier(original)

	got := c.GetRules()
	require.Len(t, got, 1)
	assert.Equal(t, "test", got[0].Description)

	got[0].Description = "mutated"
	got2 := c.GetRules()
	assert.Equal(t, "test", got2[0].Description)
}

func TestClassifier_DefaultDescription(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}},
	})

	_, _, reason := c.Classify(AttemptEvent{Success: false, StatusCode: 400})
	assert.Equal(t, "rule_match", reason, "should use default description when empty")
}

func TestClassifier_ModelSpecificRules(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{Model: "gpt-4", StatusCodes: []int{400}, Description: "gpt4_client_error"},
		{Model: "default", StatusCodes: []int{400}, Level: 2, Description: "default_client_error"},
	})

	excl, level, reason := c.Classify(AttemptEvent{Success: false, ModelName: "gpt-4", StatusCode: 400})
	assert.True(t, excl)
	assert.Equal(t, 0, level)
	assert.Equal(t, "gpt4_client_error", reason)

	excl2, level2, reason2 := c.Classify(AttemptEvent{Success: false, ModelName: "claude-3", StatusCode: 400})
	assert.False(t, excl2)
	assert.Equal(t, 2, level2)
	assert.Equal(t, "default_client_error", reason2)
}

func TestClassifier_LevelScoring(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}, Level: 0, Description: "excluded"},
		{StatusCodes: []int{429}, Level: 2, Description: "serious"},
		{StatusCodes: []int{503}, Level: 3, Description: "critical"},
	})

	_, level0, _ := c.Classify(AttemptEvent{Success: false, StatusCode: 400})
	assert.Equal(t, 0, level0)

	_, level2, _ := c.Classify(AttemptEvent{Success: false, StatusCode: 429})
	assert.Equal(t, 2, level2)

	_, level3, _ := c.Classify(AttemptEvent{Success: false, StatusCode: 503})
	assert.Equal(t, 3, level3)
}

func TestClassifier_TaskFailReason(t *testing.T) {
	t.Parallel()
	c := NewRuleBasedClassifier([]ErrorExclusionRule{
		{MessageKeywords: []string{"invalid parameter"}, Description: "param_error"},
		{Model: "midjourney", MessageKeywords: []string{"banned prompt"}, Description: "mj_banned"},
	})

	excl, _, reason := c.ClassifyTaskFailReason("midjourney", "Your banned prompt was detected")
	assert.True(t, excl)
	assert.Equal(t, "mj_banned", reason)

	excl2, _, reason2 := c.ClassifyTaskFailReason("suno", "invalid parameter: style")
	assert.True(t, excl2)
	assert.Equal(t, "param_error", reason2)

	excl3, _, _ := c.ClassifyTaskFailReason("suno", "upstream timeout")
	assert.False(t, excl3)
}
