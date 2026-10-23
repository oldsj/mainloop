package tasks

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/oldsj/mainloop/server/internal/config"
)

func validateOverrides(profile config.ProviderProfile, model, effort *string) error {
	models := make([]string, 0, len(profile.Models))
	for _, offered := range profile.Models {
		models = append(models, offered.ID)
	}
	var invalid []string
	if model != nil && !slices.Contains(models, *model) {
		invalid = append(invalid, fmt.Sprintf("model %q; allowed models: %v", *model, models))
	}
	if effort != nil && !slices.Contains(profile.EffortLevels, *effort) {
		invalid = append(invalid, fmt.Sprintf("effort %q; allowed effort_levels: %v", *effort, profile.EffortLevels))
	}
	if len(invalid) > 0 {
		return &Error{Status: 422, Reason: ReasonInvalidRequest, Detail: fmt.Sprintf("provider %s: %v", profile.ID, invalid)}
	}
	return nil
}

func initialOverrides(payload json.RawMessage, model, effort *string) (*string, *string, error) {
	var request struct {
		Model  *string `json:"model"`
		Effort *string `json:"effort"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, nil, err
	}
	if request.Model != nil {
		model = request.Model
	}
	if request.Effort != nil {
		effort = request.Effort
	}
	return model, effort, nil
}
