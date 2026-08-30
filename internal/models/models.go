// Package models defines the request, response and error types exchanged over
// the worker HTTP API. It has no dependencies on any other internal package so
// that every other component can safely import it.
package models

// BrowserRequest is the POST /v1/tasks request body.
type BrowserRequest struct {
	TaskID            string `json:"task_id"`
	URL               string `json:"url"`
	CaptureScreenshot bool   `json:"capture_screenshot"`
	MaxTextChars      int    `json:"max_text_chars"`
}

// VisibleLink is one extracted link.
type VisibleLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// BrowserResult is the POST /v1/tasks response body.
type BrowserResult struct {
	TaskID               string        `json:"task_id"`
	Status               string        `json:"status"`
	FinalURL             string        `json:"final_url,omitempty"`
	Title                string        `json:"title,omitempty"`
	VisibleText          string        `json:"visible_text,omitempty"`
	Links                []VisibleLink `json:"links,omitempty"`
	ScreenshotArtifactID string        `json:"screenshot_artifact_id,omitempty"`
	Error                *BrowserError `json:"error,omitempty"`
}

// BrowserError is the machine-readable error type.
type BrowserError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *BrowserError) Error() string { return e.Code + ": " + e.Message }

// NewError builds a *BrowserError.
func NewError(code, message string) *BrowserError {
	return &BrowserError{Code: code, Message: message}
}

// Task statuses.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Error codes.
const (
	CodeInvalidRequest        = "invalid_request"
	CodeInvalidURL            = "invalid_url"
	CodeBlockedDestination    = "blocked_destination"
	CodeDNSFailure            = "dns_failure"
	CodeRedirectBlocked       = "redirect_blocked"
	CodeRedirectLimitExceeded = "redirect_limit_exceeded"
	CodeBrowserStartFailed    = "browser_start_failed"
	CodeNavigationTimeout     = "navigation_timeout"
	CodeExtractionFailed      = "extraction_failed"
	CodeScreenshotFailed      = "screenshot_failed"
	CodeTaskTimeout           = "task_timeout"
	CodeOverloaded            = "overloaded"
)

// HTTPStatusForCode maps an error code to the HTTP status defined by the API
// contract. Unknown codes map to 500.
func HTTPStatusForCode(code string) int {
	switch code {
	case CodeInvalidRequest,
		CodeInvalidURL,
		CodeBlockedDestination,
		CodeDNSFailure,
		CodeRedirectBlocked,
		CodeRedirectLimitExceeded:
		return 400
	case CodeOverloaded:
		return 503
	case CodeNavigationTimeout, CodeTaskTimeout:
		return 504
	case CodeBrowserStartFailed, CodeExtractionFailed, CodeScreenshotFailed:
		return 500
	default:
		return 500
	}
}
