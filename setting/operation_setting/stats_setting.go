package operation_setting

// StatsErrorExclusionRulesJSON holds the raw JSON for stats error exclusion rules.
// Parsed and applied by service.InitRelayStats via the OnStatsExclusionRulesUpdate callback.
// Default rules exclude common non-channel-fault errors from statistics.
var StatsErrorExclusionRulesJSON = `[
  {"status_codes":[400,422],"error_codes":["invalid_request","invalid_parameter","context_length_exceeded","bad_request_body"],"level":0,"description":"Client parameter errors"},
  {"status_codes":[429],"level":0,"description":"Rate limiting (transient, not a channel fault)"},
  {"error_codes":["sensitive_words_detected","prompt_blocked","access_denied"],"message_keywords":["safety","blocked","content policy","content_policy"],"level":0,"description":"Content policy / safety blocks"},
  {"error_codes":["insufficient_user_quota","pre_consume_token_quota_failed"],"level":0,"description":"User quota errors (not channel fault)"},
  {"channel_types":[24],"error_codes":["prompt_blocked"],"message_keywords":["safety","blocked","recitation"],"level":0,"description":"Gemini safety blocks"}
]`

// OnStatsExclusionRulesUpdate is a callback set by the service layer to apply
// rule updates without creating a circular dependency (model -> service).
var OnStatsExclusionRulesUpdate func(jsonStr string) error

func StatsErrorExclusionRulesToString() string {
	return StatsErrorExclusionRulesJSON
}

func StatsErrorExclusionRulesFromString(s string) error {
	StatsErrorExclusionRulesJSON = s
	if OnStatsExclusionRulesUpdate != nil {
		return OnStatsExclusionRulesUpdate(s)
	}
	return nil
}
