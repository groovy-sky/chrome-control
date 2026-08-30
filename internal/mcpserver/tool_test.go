package mcpserver_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/groovy-sky/chrome-control/internal/mcpserver"
	"github.com/groovy-sky/chrome-control/internal/models"
)

// fakeExecutor is a test double for the browser executor.
type fakeExecutor struct {
	result *models.BrowserResult
	// capturedReq holds the last request passed to Run.
	capturedReq models.BrowserRequest
}

func (f *fakeExecutor) Run(_ context.Context, req models.BrowserRequest) *models.BrowserResult {
	f.capturedReq = req
	if f.result != nil {
		return f.result
	}
	return &models.BrowserResult{
		TaskID:   req.TaskID,
		Status:   models.StatusCompleted,
		FinalURL: req.URL,
		Title:    "Test Page",
	}
}

// callHandler invokes the browse_url handler with the given arguments and
// returns the MCP result.
func callHandler(t *testing.T, exec mcpserver.Executor, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	handler := mcpserver.Handler(exec)
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	var req mcp.CallToolRequest
	if err := json.Unmarshal([]byte(`{"method":"tools/call","params":{"name":"browse_url","arguments":`+string(raw)+`}}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned unexpected error: %v", err)
	}
	return result
}

func TestHandler_SuccessfulBrowse(t *testing.T) {
	exec := &fakeExecutor{}
	result := callHandler(t, exec, map[string]any{
		"url":     "https://example.com",
		"task_id": "my-task-123",
	})

	if result.IsError {
		t.Fatalf("expected success, got error: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}
	text := result.Content[0].(mcp.TextContent).Text
	var got models.BrowserResult
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got.TaskID != "my-task-123" {
		t.Errorf("task_id: got %q, want %q", got.TaskID, "my-task-123")
	}
	if got.Status != models.StatusCompleted {
		t.Errorf("status: got %q, want %q", got.Status, models.StatusCompleted)
	}
}

func TestHandler_GeneratesTaskIDWhenOmitted(t *testing.T) {
	exec := &fakeExecutor{}
	callHandler(t, exec, map[string]any{"url": "https://example.com"})

	if exec.capturedReq.TaskID == "" {
		t.Error("expected a generated task ID, got empty string")
	}
	// Generated IDs are 32 hex chars (16 bytes).
	if len(exec.capturedReq.TaskID) != 32 {
		t.Errorf("expected 32-char task ID, got %d chars", len(exec.capturedReq.TaskID))
	}
}

func TestHandler_PropagatesOptionalFields(t *testing.T) {
	exec := &fakeExecutor{}
	callHandler(t, exec, map[string]any{
		"url":                "https://example.com",
		"capture_screenshot": true,
		"max_text_chars":     500,
	})

	if !exec.capturedReq.CaptureScreenshot {
		t.Error("expected capture_screenshot=true")
	}
	if exec.capturedReq.MaxTextChars != 500 {
		t.Errorf("max_text_chars: got %d, want 500", exec.capturedReq.MaxTextChars)
	}
}

func TestHandler_BrowserErrorMapsToMCPError(t *testing.T) {
	exec := &fakeExecutor{
		result: &models.BrowserResult{
			TaskID: "t1",
			Status: models.StatusFailed,
			Error:  models.NewError(models.CodeInvalidURL, "bad url"),
		},
	}
	result := callHandler(t, exec, map[string]any{
		"url":     "not-a-url",
		"task_id": "t1",
	})

	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, models.CodeInvalidURL) {
		t.Errorf("expected error code %q in response text, got: %s", models.CodeInvalidURL, text)
	}
}

func TestHandler_OverloadedErrorCode(t *testing.T) {
	exec := &fakeExecutor{
		result: &models.BrowserResult{
			TaskID: "t2",
			Status: models.StatusFailed,
			Error:  models.NewError(models.CodeOverloaded, "too busy"),
		},
	}
	result := callHandler(t, exec, map[string]any{"url": "https://x.com", "task_id": "t2"})
	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, models.CodeOverloaded) {
		t.Errorf("expected %q in error text, got: %s", models.CodeOverloaded, text)
	}
}

func TestHandler_HandlerDoesNotReturnGoError(t *testing.T) {
	// The handler must always return (result, nil); MCP errors go into
	// result.IsError so the protocol stays intact.
	exec := &fakeExecutor{
		result: &models.BrowserResult{
			Status: models.StatusFailed,
			Error:  models.NewError(models.CodeBrowserStartFailed, "chrome failed"),
		},
	}
	handler := mcpserver.Handler(exec)
	var req mcp.CallToolRequest
	_ = json.Unmarshal([]byte(`{"params":{"arguments":{"url":"https://x.com","task_id":"t3"}}}`), &req)
	_, goErr := handler(context.Background(), req)
	if goErr != nil {
		t.Errorf("expected nil Go error, got %v", goErr)
	}
}
