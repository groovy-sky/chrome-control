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

// SessionRequest is the POST /v1/sessions request body.
type SessionRequest struct {
	// URL is optional for interactive sessions only. When omitted or empty,
	// Chromium starts on about:blank and waits for manual navigation through
	// noVNC. When provided, it must still satisfy the normal destination policy.
	URL               string `json:"url"`
	CaptureScreenshot bool   `json:"capture_screenshot"`
	MaxTextChars      int    `json:"max_text_chars"`
}

// SessionResponse is returned after a session is created successfully.
type SessionResponse struct {
	Token  string `json:"token"`
	Status string `json:"status"`
}

// SessionStatus is returned by GET /v1/sessions/{token}.
type SessionStatus struct {
	Token  string         `json:"token"`
	Status string         `json:"status"`
	Result *BrowserResult `json:"result,omitempty"`
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
	StatusWaiting   = "waiting"
	StatusCancelled = "cancelled"
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
	CodeSessionNotFound       = "session_not_found"
	CodeSessionExpired        = "session_expired"
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
	case CodeSessionNotFound:
		return 404
	case CodeSessionExpired:
		return 410
	case CodeNavigationTimeout, CodeTaskTimeout:
		return 504
	case CodeBrowserStartFailed, CodeExtractionFailed, CodeScreenshotFailed:
		return 500
	default:
		return 500
	}
}
