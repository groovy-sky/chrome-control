package unit

import (
	"testing"

	"github.com/groovy-sky/chrome-control/internal/flows"
	"github.com/groovy-sky/chrome-control/internal/models"
)

func validFlow() flows.Flow {
	return flows.Flow{
		Version: flows.SupportedVersion,
		Name:    "login",
		Steps: []flows.Step{
			{ID: "s1", Type: flows.StepNavigate, URL: "https://example.com/login"},
			{ID: "s2", Type: flows.StepFill, Locator: &flows.Locator{Strategy: flows.LocatorCSS, Value: "#user"}, Value: "alice"},
			{ID: "s3", Type: flows.StepClick, Locator: &flows.Locator{Strategy: flows.LocatorCSS, Value: "#submit"}},
			{ID: "s4", Type: flows.StepAssertURL, URL: "https://example.com/dashboard"},
		},
	}
}

func TestValidate_ValidFlow(t *testing.T) {
	t.Parallel()
	f := validFlow()
	if berr := flows.Validate(&f); berr != nil {
		t.Fatalf("expected valid flow, got error: %+v", berr)
	}
}

func TestValidate_NilFlow(t *testing.T) {
	t.Parallel()
	if berr := flows.Validate(nil); berr == nil {
		t.Fatal("expected error for nil flow")
	}
}

func TestValidate_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Version = 2
	berr := flows.Validate(&f)
	if berr == nil || berr.Code != models.CodeInvalidRequest {
		t.Fatalf("expected invalid_request for unsupported version, got %+v", berr)
	}
}

func TestValidate_EmptyName(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Name = ""
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for empty flow name")
	}
}

func TestValidate_NameTooLong(t *testing.T) {
	t.Parallel()
	f := validFlow()
	longName := ""
	for i := 0; i < flows.MaxFlowNameLength+1; i++ {
		longName += "a"
	}
	f.Name = longName
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for oversized flow name")
	}
}

func TestValidate_NoSteps(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps = nil
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for flow with no steps")
	}
}

func TestValidate_TooManySteps(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps = make([]flows.Step, flows.MaxSteps+1)
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for too many steps")
	}
}

func TestValidate_DuplicateStepIDs(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[1].ID = f.Steps[0].ID
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for duplicate step ids")
	}
}

func TestValidate_UnknownStepType(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[0].Type = "run_javascript"
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for unsupported step type")
	}
}

func TestValidate_MissingLocatorForClick(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[2].Locator = nil
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for missing locator on click step")
	}
}

func TestValidate_LocatorNotPermittedForNavigate(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[0].Locator = &flows.Locator{Strategy: flows.LocatorCSS, Value: "#x"}
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for locator on navigate step")
	}
}

func TestValidate_UnsupportedLocatorStrategy(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[2].Locator = &flows.Locator{Strategy: "xpath", Value: "//button"}
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for xpath locator strategy")
	}
}

func TestValidate_LocatorValueEmpty(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[2].Locator = &flows.Locator{Strategy: flows.LocatorCSS, Value: ""}
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for empty locator value")
	}
}

func TestValidate_LocatorValueTooLong(t *testing.T) {
	t.Parallel()
	f := validFlow()
	longSel := ""
	for i := 0; i < flows.MaxSelectorLength+1; i++ {
		longSel += "a"
	}
	f.Steps[2].Locator = &flows.Locator{Strategy: flows.LocatorCSS, Value: longSel}
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for oversized locator value")
	}
}

func TestValidate_FillValueTooLong(t *testing.T) {
	t.Parallel()
	f := validFlow()
	longVal := ""
	for i := 0; i < flows.MaxValueLength+1; i++ {
		longVal += "a"
	}
	f.Steps[1].Value = longVal
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for oversized fill value")
	}
}

func TestValidate_ValueNotPermittedForClick(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[2].Value = "unexpected"
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for value on click step")
	}
}

func TestValidate_NavigateRequiresURL(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[0].URL = ""
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for navigate step without url")
	}
}

func TestValidate_NavigateRejectsNonHTTPSURL(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[0].URL = "http://example.com"
	berr := flows.Validate(&f)
	if berr == nil {
		t.Fatal("expected error for non-https navigate url")
	}
}

func TestValidate_NavigateRejectsPrivateHost(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[0].URL = "https://localhost/"
	berr := flows.Validate(&f)
	if berr == nil || berr.Code != models.CodeBlockedDestination {
		t.Fatalf("expected blocked_destination for localhost, got %+v", berr)
	}
}

func TestValidate_AssertURLRequiresURL(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[3].URL = ""
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for assert_url step without url")
	}
}

func TestValidate_URLNotPermittedForClick(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[2].URL = "https://example.com"
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for url on click step")
	}
}

func TestValidate_StepIDEmpty(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[0].ID = ""
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for empty step id")
	}
}

func TestValidate_TimeoutOutOfBounds(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[2].TimeoutMs = flows.MaxStepTimeoutMs + 1
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for timeout above bound")
	}

	f2 := validFlow()
	f2.Steps[2].TimeoutMs = flows.MinStepTimeoutMs - 1
	if berr := flows.Validate(&f2); berr == nil {
		t.Fatal("expected error for timeout below bound")
	}
}

func TestValidate_StartURLValidatedLikeNavigate(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.StartURL = "ftp://example.com"
	if berr := flows.Validate(&f); berr == nil {
		t.Fatal("expected error for non-https start_url")
	}
}

func TestValidate_SelectRequiresLocatorButAllowsEmptyValue(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps = append(f.Steps, flows.Step{
		ID:      "s5",
		Type:    flows.StepSelect,
		Locator: &flows.Locator{Strategy: flows.LocatorName, Value: "country"},
		Value:   "",
	})
	if berr := flows.Validate(&f); berr != nil {
		t.Fatalf("expected select with empty value to be valid, got %+v", berr)
	}
}

func TestValidate_IDAndNameLocatorStrategiesAccepted(t *testing.T) {
	t.Parallel()
	f := validFlow()
	f.Steps[2].Locator = &flows.Locator{Strategy: flows.LocatorID, Value: "submit-btn"}
	if berr := flows.Validate(&f); berr != nil {
		t.Fatalf("expected id locator strategy to be valid, got %+v", berr)
	}
	f.Steps[2].Locator = &flows.Locator{Strategy: flows.LocatorName, Value: "submit"}
	if berr := flows.Validate(&f); berr != nil {
		t.Fatalf("expected name locator strategy to be valid, got %+v", berr)
	}
}
