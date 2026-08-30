// Package mcpserver implements the MCP browse_url tool that delegates to the
// browser worker. The Executor interface allows the handler to be unit-tested
// without Chromium.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/groovy-sky/chrome-control/internal/models"
)

// Executor runs one browser task and returns the result.
// Implementations must always return a non-nil *models.BrowserResult;
// failures are encoded in result.Error rather than as a Go error return value.
type Executor interface {
	Run(ctx context.Context, req models.BrowserRequest) *models.BrowserResult
}

// BrowseURLTool is the MCP tool definition for browse_url.
var BrowseURLTool = mcp.NewTool("browse_url",
	mcp.WithDescription("Browse a public HTTPS URL using a headless Chromium instance. "+
		"Returns the page title, visible text, links, final URL, and optionally a screenshot artifact identifier. "+
		"Only publicly routable HTTPS destinations are permitted; private/internal addresses are blocked."),
	mcp.WithString("url",
		mcp.Required(),
		mcp.Description("The URL to visit. Must be a publicly routable HTTPS URL.")),
	mcp.WithBoolean("capture_screenshot",
		mcp.Description("When true, capture a PNG screenshot and return its artifact identifier.")),
	mcp.WithNumber("max_text_chars",
		mcp.Description("Maximum number of characters to include in visible_text. 0 means unlimited.")),
	mcp.WithString("task_id",
		mcp.Description("Optional opaque task identifier (1-128 characters). "+
			"A random identifier is generated when omitted.")),
)

// Handler returns a server.ToolHandlerFunc that handles browse_url calls.
func Handler(exec Executor) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		taskID := req.GetString("task_id", "")
		if taskID == "" {
			id, err := generateTaskID()
			if err != nil {
				return errorResult(models.CodeInvalidRequest, "could not generate task ID"), nil
			}
			taskID = id
		}

		url := req.GetString("url", "")
		captureScreenshot := req.GetBool("capture_screenshot", false)
		maxTextChars := req.GetInt("max_text_chars", 0)

		browserReq := models.BrowserRequest{
			TaskID:            taskID,
			URL:               url,
			CaptureScreenshot: captureScreenshot,
			MaxTextChars:      maxTextChars,
		}

		result := exec.Run(ctx, browserReq)
		return buildResult(result), nil
	}
}

// RegisterTool registers the browse_url tool with the given MCP server.
func RegisterTool(s *server.MCPServer, exec Executor) {
	s.AddTool(BrowseURLTool, Handler(exec))
}

// buildResult converts a BrowserResult into an MCP CallToolResult.
func buildResult(result *models.BrowserResult) *mcp.CallToolResult {
	if result.Error != nil {
		return errorResult(result.Error.Code, result.Error.Message)
	}

	data, err := json.Marshal(result)
	if err != nil {
		return errorResult(models.CodeExtractionFailed, "could not serialize result")
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent(string(data)),
		},
	}
}

// errorResult returns an MCP tool error result with a structured JSON payload
// so callers can reliably extract the error code without string parsing.
func errorResult(code, message string) *mcp.CallToolResult {
	payload, err := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		Error: struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: code, Message: message},
	})
	if err != nil {
		// Fallback: include the raw code as plain text so nothing is silently lost.
		payload = []byte(`{"error":{"code":"` + code + `"}}`)
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			mcp.NewTextContent(string(payload)),
		},
	}
}

// generateTaskID returns a 16-byte hex-encoded random opaque identifier.
func generateTaskID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
