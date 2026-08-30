//go:build integration

// Package integration holds tests that require a real Chromium binary. They
// are excluded from the default build and run with:
//
//	go test -tags integration ./tests/integration/...
//
// Tests that need to reach the public internet are skipped unless
// CHROME_CONTROL_IT_URL points at a controlled public HTTPS page.
package integration

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/groovy-sky/chrome-control/internal/artifacts"
	"github.com/groovy-sky/chrome-control/internal/browser"
	"github.com/groovy-sky/chrome-control/internal/models"
)

func chromePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CHROME_PATH"); p != "" {
		return p
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "headless_shell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no chromium binary found; set CHROME_PATH")
	return ""
}

func newWorker(t *testing.T, cfg browser.Config) *browser.Worker {
	t.Helper()
	if cfg.ChromePath == "" {
		cfg.ChromePath = chromePath(t)
	}
	if cfg.Artifacts == nil {
		store, err := artifacts.New(filepath.Join(t.TempDir(), "artifacts"))
		if err != nil {
			t.Fatalf("artifact store: %v", err)
		}
		cfg.Artifacts = store
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return browser.New(cfg)
}

func profileDirs(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "chrome-control-profile-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

// chromeProcesses counts live processes whose command line mentions a
// chrome-control profile directory, i.e. Chromium processes this worker
// started.
func chromeProcesses(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skip("/proc is not available on this platform")
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "chrome-control-profile-") {
			count++
		}
	}
	return count
}

func TestProbeStartsChromiumWithSandbox(t *testing.T) {
	w := newWorker(t, browser.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.Probe(ctx); err != nil {
		t.Fatalf("chromium could not be started: %v", err)
	}
}

// TestRequestInterceptionEnabledAfterStartup reproduces the regression where
// canceling the startup child context invalidated the browser target, causing
// fetch.Enable to return "could not enable request interception" on every task.
func TestRequestInterceptionEnabledAfterStartup(t *testing.T) {
	w := newWorker(t, browser.Config{
		Resolver: publicResolver{},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The request is blocked by policy (publicResolver returns a public IP,
	// but the local test server URL would normally be blocked). We use a
	// well-known blocked destination so the test does not need internet access
	// but still exercises the full startup → interception path.
	res := w.Run(ctx, models.BrowserRequest{
		TaskID: "interception-regression",
		URL:    "https://127.0.0.1/",
	})
	// The task must fail due to policy (blocked destination), NOT due to
	// browser_start_failed / "could not enable request interception".
	if res.Error == nil {
		t.Fatalf("expected a policy-blocked failure, got success: %+v", res)
	}
	if res.Error.Code == models.CodeBrowserStartFailed {
		t.Fatalf("regression: got browser_start_failed (%q); request interception must be enabled after startup", res.Error.Message)
	}
}

func TestTaskBlocksLocalHTTPSServer(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()

	w := newWorker(t, browser.Config{})
	res := w.Run(context.Background(), models.BrowserRequest{
		TaskID: "blocked-local",
		URL:    srv.URL,
	})
	if res.Status != models.StatusFailed || res.Error == nil {
		t.Fatalf("expected the local server to be blocked, got %+v", res)
	}
	if res.VisibleText != "" || res.Title != "" || res.ScreenshotArtifactID != "" {
		t.Fatalf("blocked task must fail closed, got %+v", res)
	}
}

func TestTaskBlocksNonPublicDestinations(t *testing.T) {
	cases := []struct {
		name string
		url  string
		code string
	}{
		{"localhost", "https://localhost/", models.CodeBlockedDestination},
		{"sub localhost", "https://api.localhost/", models.CodeBlockedDestination},
		{"metadata", "https://metadata.google.internal/", models.CodeBlockedDestination},
		{"metadata ip", "https://169.254.169.254/", models.CodeBlockedDestination},
		{"loopback literal", "https://127.0.0.1/", models.CodeBlockedDestination},
		{"private literal", "https://10.0.0.1/", models.CodeBlockedDestination},
		{"ipv4 mapped ipv6", "https://[::ffff:10.0.0.1]/", models.CodeBlockedDestination},
		{"plain http", "http://example.com/", models.CodeInvalidURL},
		{"non standard port", "https://example.com:8443/", models.CodeInvalidURL},
	}
	w := newWorker(t, browser.Config{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := w.Run(context.Background(), models.BrowserRequest{TaskID: "t", URL: tc.url})
			if res.Error == nil {
				t.Fatalf("expected rejection, got %+v", res)
			}
			if res.Error.Code != tc.code {
				t.Fatalf("got code %q, want %q", res.Error.Code, tc.code)
			}
		})
	}
}

func TestProfileDirectoryRemovedAfterTask(t *testing.T) {
	before := len(profileDirs(t))

	w := newWorker(t, browser.Config{})
	url := os.Getenv("CHROME_CONTROL_IT_URL")
	if url == "" {
		url = "https://127.0.0.1/"
	}
	_ = w.Run(context.Background(), models.BrowserRequest{TaskID: "cleanup", URL: url})

	if after := len(profileDirs(t)); after > before {
		t.Fatalf("temporary profile directories leaked: before=%d after=%d", before, after)
	}
}

func TestChromiumProcessTreeTerminated(t *testing.T) {
	before := chromeProcesses(t)

	w := newWorker(t, browser.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}

	// Allow the kernel a moment to reap the process group.
	time.Sleep(500 * time.Millisecond)
	if after := chromeProcesses(t); after > before {
		t.Fatalf("chromium processes leaked: before=%d after=%d", before, after)
	}
}

func TestBrowserStartFailureIsReported(t *testing.T) {
	w := newWorker(t, browser.Config{
		ChromePath: filepath.Join(t.TempDir(), "does-not-exist"),
		Resolver:   publicResolver{},
	})
	before := len(profileDirs(t))

	res := w.Run(context.Background(), models.BrowserRequest{
		TaskID: "start-failure",
		URL:    "https://example.com/",
	})
	if res.Error == nil || res.Error.Code != models.CodeBrowserStartFailed {
		t.Fatalf("expected browser_start_failed, got %+v", res)
	}
	if after := len(profileDirs(t)); after > before {
		t.Fatalf("profile directory leaked after startup failure: before=%d after=%d", before, after)
	}
}

func TestClientCancellationStopsTask(t *testing.T) {
	requirePublicURL(t)
	w := newWorker(t, browser.Config{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := w.Run(ctx, models.BrowserRequest{TaskID: "cancelled", URL: os.Getenv("CHROME_CONTROL_IT_URL")})
	if res.Error == nil {
		t.Fatalf("expected the cancelled task to fail, got %+v", res)
	}
}

func TestTaskTimeoutTerminatesTask(t *testing.T) {
	requirePublicURL(t)
	w := newWorker(t, browser.Config{TaskTimeout: time.Millisecond})

	res := w.Run(context.Background(), models.BrowserRequest{
		TaskID: "timeout",
		URL:    os.Getenv("CHROME_CONTROL_IT_URL"),
	})
	if res.Error == nil {
		t.Fatalf("expected a timeout error, got %+v", res)
	}
}

func TestExtractionAndScreenshot(t *testing.T) {
	requirePublicURL(t)
	store, err := artifacts.New(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	w := newWorker(t, browser.Config{Artifacts: store})

	res := w.Run(context.Background(), models.BrowserRequest{
		TaskID:            "extract",
		URL:               os.Getenv("CHROME_CONTROL_IT_URL"),
		CaptureScreenshot: true,
		MaxTextChars:      500,
	})
	if res.Error != nil {
		t.Fatalf("task failed: %+v", res.Error)
	}
	if res.Status != models.StatusCompleted {
		t.Fatalf("got status %q, want %q", res.Status, models.StatusCompleted)
	}
	if res.FinalURL == "" {
		t.Error("final_url must be populated")
	}
	if len([]rune(res.VisibleText)) > 500 {
		t.Errorf("visible_text exceeds max_text_chars: %d code points", len([]rune(res.VisibleText)))
	}
	if len(res.Links) > browser.MaxLinks {
		t.Errorf("got %d links, want at most %d", len(res.Links), browser.MaxLinks)
	}
	if res.ScreenshotArtifactID == "" {
		t.Fatal("screenshot artifact id must be populated")
	}

	path := filepath.Join(store.Root(), res.ScreenshotArtifactID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("screenshot artifact missing: %v", err)
	}
	if info.Size() > artifacts.MaxFileSizeBytes {
		t.Errorf("screenshot is %d bytes, above the %d byte limit", info.Size(), artifacts.MaxFileSizeBytes)
	}
	if err := store.Delete(res.ScreenshotArtifactID); err != nil {
		t.Fatalf("artifact delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("artifact must be removed after the response is sent")
	}
}

func requirePublicURL(t *testing.T) {
	t.Helper()
	if os.Getenv("CHROME_CONTROL_IT_URL") == "" {
		t.Skip("set CHROME_CONTROL_IT_URL to a controlled public HTTPS page to run this test")
	}
}

// publicResolver resolves every name to a public address so that tests which
// exercise the browser lifecycle do not depend on external DNS.
type publicResolver struct{}

func (publicResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}
