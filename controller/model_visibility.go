package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// resolveVisibleModelNames returns the models visible to the current caller,
// respecting token model limits and token-group scoping in the same way as the
// model list endpoint.
func resolveVisibleModelNames(c *gin.Context) ([]string, error) {
	if common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		s, ok := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
		if !ok {
			return []string{}, nil
		}
		tokenModelLimit, ok := s.(map[string]bool)
		if !ok {
			return []string{}, nil
		}
		models := make([]string, 0, len(tokenModelLimit))
		for allowModel := range tokenModelLimit {
			models = append(models, allowModel)
		}
		return models, nil
	}

	userGroup := c.GetString("group")
	if userGroup == "" {
		userId := c.GetInt("id")
		var err error
		userGroup, err = model.GetUserGroup(userId, false)
		if err != nil {
			return nil, err
		}
	}

	tokenGroup := common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
	if tokenGroup == "auto" {
		models := make([]string, 0)
		for _, autoGroup := range service.GetUserAutoGroup(userGroup) {
			groupModels := model.GetGroupEnabledModels(autoGroup)
			for _, groupModel := range groupModels {
				if !common.StringsContains(models, groupModel) {
					models = append(models, groupModel)
				}
			}
		}
		return models, nil
	}

	group := userGroup
	if tokenGroup != "" {
		group = tokenGroup
	}
	return model.GetGroupEnabledModels(group), nil
}
