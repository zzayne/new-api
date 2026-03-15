package service

import (
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// ErrorExclusionRule defines a rule for classifying errors.
// Level 0 = excluded from stats, 1 = normal (default), 2 = serious, 3 = critical.
// Model field supports per-model rules; empty or "default" applies to all models.
type ErrorExclusionRule struct {
	Model           string   `json:"model,omitempty"`
	ChannelTypes    []int    `json:"channel_types,omitempty"`
	ErrorCodes      []string `json:"error_codes,omitempty"`
	StatusCodes     []int    `json:"status_codes,omitempty"`
	MessageKeywords []string `json:"message_keywords,omitempty"`
	Level           int      `json:"level"`
	Description     string   `json:"description,omitempty"`
}

func (r *ErrorExclusionRule) isDefault() bool {
	return r.Model == "" || strings.EqualFold(r.Model, "default")
}

// ruleGroup holds pre-built lookup structures for a set of rules.
type ruleGroup struct {
	rules           []ErrorExclusionRule
	statusCodeSets  []map[int]struct{}
	errorCodeSets   []map[string]struct{}
	channelTypeSets []map[int]struct{}
}

// RuleBasedClassifier classifies errors using configurable rules indexed by model.
type RuleBasedClassifier struct {
	mu           sync.RWMutex
	allRules     []ErrorExclusionRule
	modelGroups  map[string]*ruleGroup // model name -> rules (exact match)
	defaultGroup *ruleGroup            // fallback rules (model="" or "default")
}

func NewRuleBasedClassifier(rules []ErrorExclusionRule) *RuleBasedClassifier {
	c := &RuleBasedClassifier{}
	c.buildRules(rules)
	return c
}

func (c *RuleBasedClassifier) Classify(event AttemptEvent) (bool, int, string) {
	if event.Success {
		return false, 0, ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Try model-specific rules first
	if event.ModelName != "" {
		if group, ok := c.modelGroups[event.ModelName]; ok {
			if excluded, level, reason := c.matchGroup(group, event.ChannelType, event.StatusCode, event.ErrorCode, event.ErrorMessage); excluded || level > 0 {
				return excluded, level, reason
			}
		}
	}
	// Fallback to default rules
	if c.defaultGroup != nil {
		return c.matchGroup(c.defaultGroup, event.ChannelType, event.StatusCode, event.ErrorCode, event.ErrorMessage)
	}
	return false, 1, ""
}

// ClassifyTaskFailReason classifies an async task's fail_reason string.
func (c *RuleBasedClassifier) ClassifyTaskFailReason(modelName string, failReason string) (bool, int, string) {
	if failReason == "" {
		return false, 1, ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Try model-specific rules (keyword match on failReason)
	if modelName != "" {
		if group, ok := c.modelGroups[modelName]; ok {
			if excluded, level, reason := c.matchGroupByMessage(group, failReason); excluded || level > 0 {
				return excluded, level, reason
			}
		}
	}
	if c.defaultGroup != nil {
		return c.matchGroupByMessage(c.defaultGroup, failReason)
	}
	return false, 1, ""
}

func (c *RuleBasedClassifier) UpdateRules(rules []ErrorExclusionRule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buildRulesLocked(rules)
}

func (c *RuleBasedClassifier) GetRules() []ErrorExclusionRule {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ErrorExclusionRule, len(c.allRules))
	copy(out, c.allRules)
	return out
}

func (c *RuleBasedClassifier) buildRules(rules []ErrorExclusionRule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buildRulesLocked(rules)
}

func (c *RuleBasedClassifier) buildRulesLocked(rules []ErrorExclusionRule) {
	c.allRules = make([]ErrorExclusionRule, len(rules))
	copy(c.allRules, rules)

	c.modelGroups = make(map[string]*ruleGroup)
	c.defaultGroup = nil

	// Partition rules by model
	modelBuckets := make(map[string][]ErrorExclusionRule)
	var defaultRules []ErrorExclusionRule
	for _, r := range rules {
		if r.isDefault() {
			defaultRules = append(defaultRules, r)
		} else {
			modelBuckets[r.Model] = append(modelBuckets[r.Model], r)
		}
	}

	for model, rs := range modelBuckets {
		c.modelGroups[model] = buildRuleGroup(rs)
	}
	if len(defaultRules) > 0 {
		c.defaultGroup = buildRuleGroup(defaultRules)
	}
}

func buildRuleGroup(rules []ErrorExclusionRule) *ruleGroup {
	g := &ruleGroup{
		rules:           rules,
		statusCodeSets:  make([]map[int]struct{}, len(rules)),
		errorCodeSets:   make([]map[string]struct{}, len(rules)),
		channelTypeSets: make([]map[int]struct{}, len(rules)),
	}
	for i, rule := range rules {
		if len(rule.StatusCodes) > 0 {
			m := make(map[int]struct{}, len(rule.StatusCodes))
			for _, sc := range rule.StatusCodes {
				m[sc] = struct{}{}
			}
			g.statusCodeSets[i] = m
		}
		if len(rule.ErrorCodes) > 0 {
			m := make(map[string]struct{}, len(rule.ErrorCodes))
			for _, ec := range rule.ErrorCodes {
				m[strings.ToLower(ec)] = struct{}{}
			}
			g.errorCodeSets[i] = m
		}
		if len(rule.ChannelTypes) > 0 {
			m := make(map[int]struct{}, len(rule.ChannelTypes))
			for _, ct := range rule.ChannelTypes {
				m[ct] = struct{}{}
			}
			g.channelTypeSets[i] = m
		}
	}
	return g
}

// matchGroup tries to match an error against all rules in the group.
// Returns (excluded, level, reason). If no rule matches, returns (false, 1, "").
func (c *RuleBasedClassifier) matchGroup(g *ruleGroup, channelType, statusCode int, errorCode, errorMessage string) (bool, int, string) {
	for i, rule := range g.rules {
		if c.matchRuleInGroup(g, i, rule, channelType, statusCode, errorCode, errorMessage) {
			desc := rule.Description
			if desc == "" {
				desc = "rule_match"
			}
			excluded := rule.Level == 0
			level := rule.Level
			return excluded, level, desc
		}
	}
	return false, 1, ""
}

// matchGroupByMessage matches only on message keywords (for task fail_reason).
func (c *RuleBasedClassifier) matchGroupByMessage(g *ruleGroup, message string) (bool, int, string) {
	lowerMsg := strings.ToLower(message)
	for _, rule := range g.rules {
		if len(rule.MessageKeywords) > 0 {
			matched, _ := AcSearch(lowerMsg, rule.MessageKeywords, true)
			if matched {
				desc := rule.Description
				if desc == "" {
					desc = "rule_match"
				}
				excluded := rule.Level == 0
				return excluded, rule.Level, desc
			}
		}
	}
	return false, 1, ""
}

func (c *RuleBasedClassifier) matchRuleInGroup(g *ruleGroup, idx int, rule ErrorExclusionRule, channelType, statusCode int, errorCode, errorMessage string) bool {
	if ctSet := g.channelTypeSets[idx]; ctSet != nil {
		if _, ok := ctSet[channelType]; !ok {
			return false
		}
	}

	hasConditions := len(rule.ErrorCodes) > 0 || len(rule.StatusCodes) > 0 || len(rule.MessageKeywords) > 0
	if !hasConditions {
		return len(rule.ChannelTypes) > 0
	}

	if ecSet := g.errorCodeSets[idx]; ecSet != nil {
		if _, ok := ecSet[strings.ToLower(errorCode)]; ok {
			return true
		}
	}
	if scSet := g.statusCodeSets[idx]; scSet != nil {
		if _, ok := scSet[statusCode]; ok {
			return true
		}
	}
	if len(rule.MessageKeywords) > 0 && errorMessage != "" {
		matched, _ := AcSearch(strings.ToLower(errorMessage), rule.MessageKeywords, true)
		if matched {
			return true
		}
	}
	return false
}

// StatsErrorExclusionRulesFromJSON parses JSON rules and updates the global classifier.
func StatsErrorExclusionRulesFromJSON(jsonStr string) error {
	if strings.TrimSpace(jsonStr) == "" {
		if c, ok := GetErrorClassifier().(*RuleBasedClassifier); ok {
			c.UpdateRules(nil)
		}
		return nil
	}
	var rules []ErrorExclusionRule
	if err := common.Unmarshal([]byte(jsonStr), &rules); err != nil {
		return err
	}
	if c, ok := GetErrorClassifier().(*RuleBasedClassifier); ok {
		c.UpdateRules(rules)
	}
	return nil
}

func StatsErrorExclusionRulesToJSON() string {
	c, ok := GetErrorClassifier().(*RuleBasedClassifier)
	if !ok {
		return "[]"
	}
	rules := c.GetRules()
	data, err := common.Marshal(rules)
	if err != nil {
		return "[]"
	}
	return string(data)
}
