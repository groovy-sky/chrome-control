package flows

import (
	"errors"
	"fmt"

	"github.com/groovy-sky/chrome-control/internal/models"
	"github.com/groovy-sky/chrome-control/internal/security"
)

var validStepTypes = map[string]bool{
	StepNavigate:      true,
	StepClick:         true,
	StepFill:          true,
	StepSelect:        true,
	StepWaitVisible:   true,
	StepAssertVisible: true,
	StepAssertURL:     true,
	StepScreenshot:    true,
}

var validLocatorStrategies = map[string]bool{
	LocatorCSS:  true,
	LocatorID:   true,
	LocatorName: true,
}

// stepsRequiringLocator are step types for which Locator is required.
var stepsRequiringLocator = map[string]bool{
	StepClick:         true,
	StepFill:          true,
	StepSelect:        true,
	StepWaitVisible:   true,
	StepAssertVisible: true,
}

// stepsRequiringURL are step types for which URL is required.
var stepsRequiringURL = map[string]bool{
	StepNavigate:  true,
	StepAssertURL: true,
}

// stepsRequiringValue are step types for which Value is meaningful. An empty
// Value is still permitted (e.g. "fill" can clear a field).
var stepsAllowingValue = map[string]bool{
	StepFill:   true,
	StepSelect: true,
}

func invalid(format string, args ...any) *models.BrowserError {
	return models.NewError(models.CodeInvalidRequest, fmt.Sprintf(format, args...))
}

// Validate applies the complete allow-list validation policy to a flow
// document. It performs no network access; navigate/start URLs are checked
// for syntax and destination policy but not resolved via DNS here.
func Validate(f *Flow) *models.BrowserError {
	if f == nil {
		return invalid("flow is required")
	}
	if f.Version != SupportedVersion {
		return invalid("unsupported flow version %d, expected %d", f.Version, SupportedVersion)
	}
	if l := len(f.Name); l == 0 || l > MaxFlowNameLength {
		return invalid("flow name must be 1-%d characters", MaxFlowNameLength)
	}
	if f.StartURL != "" {
		if berr := validateNavigateURL(f.StartURL); berr != nil {
			return berr
		}
	}
	if len(f.Steps) == 0 {
		return invalid("flow must contain at least one step")
	}
	if len(f.Steps) > MaxSteps {
		return invalid("flow must contain at most %d steps", MaxSteps)
	}

	seenIDs := make(map[string]bool, len(f.Steps))
	for i := range f.Steps {
		step := &f.Steps[i]
		if berr := validateStep(step); berr != nil {
			return berr
		}
		if seenIDs[step.ID] {
			return invalid("duplicate step id %q", step.ID)
		}
		seenIDs[step.ID] = true
	}
	return nil
}

func validateStep(s *Step) *models.BrowserError {
	if l := len(s.ID); l == 0 || l > MaxStepIDLength {
		return invalid("step id must be 1-%d characters", MaxStepIDLength)
	}
	if !validStepTypes[s.Type] {
		return invalid("unsupported step type %q", s.Type)
	}

	if stepsRequiringLocator[s.Type] {
		if berr := validateLocator(s.Locator); berr != nil {
			return berr
		}
	} else if s.Locator != nil {
		return invalid("step type %q does not accept a locator", s.Type)
	}

	if stepsRequiringURL[s.Type] {
		if s.Type == StepNavigate {
			if berr := validateNavigateURL(s.URL); berr != nil {
				return berr
			}
		} else {
			if s.URL == "" {
				return invalid("step type %q requires url", s.Type)
			}
			if len(s.URL) > MaxURLLength {
				return invalid("url must be at most %d characters", MaxURLLength)
			}
		}
	} else if s.URL != "" {
		return invalid("step type %q does not accept a url", s.Type)
	}

	if stepsAllowingValue[s.Type] {
		if len(s.Value) > MaxValueLength {
			return invalid("value must be at most %d characters", MaxValueLength)
		}
	} else if s.Value != "" {
		return invalid("step type %q does not accept a value", s.Type)
	}

	if s.TimeoutMs != 0 && (s.TimeoutMs < MinStepTimeoutMs || s.TimeoutMs > MaxStepTimeoutMs) {
		return invalid("timeout_ms must be between %d and %d", MinStepTimeoutMs, MaxStepTimeoutMs)
	}
	return nil
}

func validateLocator(l *Locator) *models.BrowserError {
	if l == nil {
		return invalid("locator is required")
	}
	if !validLocatorStrategies[l.Strategy] {
		return invalid("unsupported locator strategy %q", l.Strategy)
	}
	if l.Value == "" || len(l.Value) > MaxSelectorLength {
		return invalid("locator value must be 1-%d characters", MaxSelectorLength)
	}
	return nil
}

// validateNavigateURL applies syntax and destination-policy validation (no
// DNS resolution) to a navigation URL, ahead of ever launching a browser.
func validateNavigateURL(raw string) *models.BrowserError {
	if raw == "" {
		return invalid("url is required")
	}
	if len(raw) > MaxURLLength {
		return invalid("url must be at most %d characters", MaxURLLength)
	}
	if _, _, err := security.ParseAndValidateURL(raw); err != nil {
		var perr *security.Error
		if errors.As(err, &perr) {
			return perr.BrowserError()
		}
		return invalid("url is not valid")
	}
	return nil
}
