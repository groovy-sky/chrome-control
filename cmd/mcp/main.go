// Command mcp exposes the isolated Chromium browser worker as an MCP server
// over stdio. Protocol messages are written to stdout; all operational logs go
// to stderr so the framing stream is never corrupted.
package main

import (
	"log/slog"
	"os"
	"path/filepath"

	mcpsdk "github.com/mark3labs/mcp-go/server"

	"github.com/groovy-sky/chrome-control/internal/artifacts"
	"github.com/groovy-sky/chrome-control/internal/browser"
	"github.com/groovy-sky/chrome-control/internal/mcpserver"
)

func main() {
	// All log output goes to stderr so stdout remains clean for MCP messages.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	artifactDir := envString("ARTIFACT_DIR", filepath.Join(os.TempDir(), "chrome-control-artifacts"))
	store, err := artifacts.New(artifactDir)
	if err != nil {
		logger.Error("could not create artifact store", slog.String("error", err.Error()))
		os.Exit(1)
	}

	w := browser.New(browser.Config{
		ChromePath: os.Getenv("CHROME_PATH"),
		Artifacts:  store,
		Logger:     logger,
	})

	s := mcpsdk.NewMCPServer("chrome-control", "1.0.0")
	mcpserver.RegisterTool(s, w)

	logger.Info("starting MCP stdio server")
	if err := mcpsdk.ServeStdio(s); err != nil {
		logger.Error("MCP server error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
