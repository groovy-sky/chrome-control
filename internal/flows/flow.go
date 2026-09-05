// Package flows defines the declarative record-and-replay flow document used
// by the flow replay runner (internal/browser) and the POST /v1/flows/run
// HTTP API. It has no dependency on chromedp so that a flow document can be
// validated without starting a browser.
//
// The MVP intentionally supports only a small, allow-listed set of step
// types and locator strategies. It does not support W3C WebDriver, arbitrary
// JavaScript execution, XPath locators, or persistent browser profiles.
package flows

import "github.com/groovy-sky/chrome-control/internal/models"

// SupportedVersion is the only flow document version accepted by this MVP.
const SupportedVersion = 1

// Step types understood by the replay runner. This list is an allow-list:
// any other value is rejected by Validate.
const (
	StepNavigate      = "navigate"
	StepClick         = "click"
	StepFill          = "fill"
	StepSelect        = "select"
	StepWaitVisible   = "wait_visible"
	StepAssertVisible = "assert_visible"
	StepAssertURL     = "assert_url"
	StepScreenshot    = "screenshot"
)

// Locator strategies understood by the replay runner. This list is an
// allow-list: XPath and arbitrary JavaScript evaluation are intentionally
// unsupported.
const (
	LocatorCSS  = "css"
	LocatorID   = "id"
	LocatorName = "name"
)

// Step result statuses.
const (
	StepStatusCompleted = "completed"
	StepStatusFailed    = "failed"
)

// Document and field bounds. These exist to keep flow documents small and to
// prevent step timeouts from bypassing the overall flow timeout.
const (
	MaxFlowNameLength = 200
	MaxStepIDLength   = 100
	MaxSteps          = 100
	MaxSelectorLength = 500
	MaxValueLength    = 4096
	MaxURLLength      = 2048

	// MinStepTimeoutMs and MaxStepTimeoutMs bound a step's optional
	// timeout_ms. A step timeout can only shorten, never extend, the
	// runner's overall flow deadline.
	MinStepTimeoutMs = 100
	MaxStepTimeoutMs = 30000
	// DefaultStepTimeoutMs is used when a step omits timeout_ms.
	DefaultStepTimeoutMs = 5000
)

// Locator identifies a single target element using an allow-listed strategy.
//
//   - "css": Value is used directly as a CSS selector.
//   - "id": Value is the element's id attribute (matched as [id="value"]).
//   - "name": Value is the element's name attribute (matched as [name="value"]).
type Locator struct {
	Strategy string `json:"strategy"`
	Value    string `json:"value"`
}

// Step is one action in a Flow. Which fields are required/permitted depends
// on Type; see Validate.
type Step struct {
	ID string `json:"id"`
	// Type is one of the Step* constants.
	Type string `json:"type"`
	// Locator identifies the target element. Required for click, fill,
	// select, wait_visible and assert_visible; not permitted otherwise.
	Locator *Locator `json:"locator,omitempty"`
	// URL is the navigation target for "navigate", or the expected current
	// URL for "assert_url". Not permitted for other step types.
	URL string `json:"url,omitempty"`
	// Value is the text to type for "fill", or the option to select
	// (by visible text or value) for "select". Not permitted otherwise.
	// Fill values are never logged.
	Value string `json:"value,omitempty"`
	// TimeoutMs optionally bounds this step. It must fall within
	// [MinStepTimeoutMs, MaxStepTimeoutMs] when set.
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// Flow is a versioned, ordered sequence of steps.
type Flow struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	// StartURL is optional. When set, the runner navigates to it before
	// executing Steps. It is redundant (but harmless) when the first step is
	// itself a "navigate" step.
	StartURL string `json:"start_url,omitempty"`
	Steps    []Step `json:"steps"`
}

// RunRequest is the POST /v1/flows/run request body.
type RunRequest struct {
	Flow Flow `json:"flow"`
	// CaptureScreenshot, when true, captures a final screenshot after a
	// successful run and returns its artifact ID. Steps of type "screenshot"
	// always capture regardless of this flag.
	CaptureScreenshot bool `json:"capture_screenshot"`
	// Label is an optional, free-form run label for logging/diagnostics. It
	// is never used as a filesystem path or identifier.
	Label string `json:"label,omitempty"`
}

// StepResult is the outcome of executing a single step.
type StepResult struct {
	ID                   string               `json:"id"`
	Type                 string               `json:"type"`
	Status               string               `json:"status"`
	DurationMs           int64                `json:"duration_ms"`
	ScreenshotArtifactID string               `json:"screenshot_artifact_id,omitempty"`
	Error                *models.BrowserError `json:"error,omitempty"`
}

// RunResult is the POST /v1/flows/run response body.
type RunResult struct {
	RunID                     string               `json:"run_id"`
	Status                    string               `json:"status"`
	FinalURL                  string               `json:"final_url,omitempty"`
	Steps                     []StepResult         `json:"steps"`
	FinalScreenshotArtifactID string               `json:"final_screenshot_artifact_id,omitempty"`
	Error                     *models.BrowserError `json:"error,omitempty"`
}
