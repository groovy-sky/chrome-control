//go:build integration

package browser

import (
	"context"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/groovy-sky/chrome-control/internal/artifacts"
	"github.com/groovy-sky/chrome-control/internal/models"
)

type denyAllResolver struct{}

func (denyAllResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, context.DeadlineExceeded
}

func integrationChromePath(t *testing.T) string {
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

func newIntegrationWorker(t *testing.T, cfg Config) *Worker {
	t.Helper()
	if cfg.ChromePath == "" {
		cfg.ChromePath = integrationChromePath(t)
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
	if cfg.BrowserStartTimeout <= 0 {
		cfg.BrowserStartTimeout = 30 * time.Second
	}
	return New(cfg)
}

func requireInteractivePublicURL(t *testing.T) string {
	t.Helper()
	rawURL := os.Getenv("CHROME_CONTROL_IT_URL")
	if rawURL == "" {
		t.Skip("set CHROME_CONTROL_IT_URL to a controlled public HTTPS page")
	}
	return rawURL
}

func requireInteractiveBrowser(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.Probe(ctx); err != nil {
		t.Skipf("chromium is not runnable in this environment: %v", err)
	}
}

func waitForPolicyViolation(t *testing.T, policy *policyState) *models.BrowserError {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if berr := policy.violation(); berr != nil {
			return berr
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for policy violation")
	return nil
}

func TestStartInteractive_BlankSessionStartsOnAboutBlank(t *testing.T) {
	w := newIntegrationWorker(t, Config{
		Resolver: denyAllResolver{},
	})
	requireInteractiveBrowser(t, w)

	res, browserCtx, cancelBrowser, cleanup, policy := w.startInteractive(context.Background(), models.SessionRequest{})
	if res != nil {
		t.Fatalf("expected blank interactive startup to succeed, got %+v", res)
	}
	defer cancelBrowser()
	defer cleanup()
	if policy == nil {
		t.Fatal("expected policy state for interactive session")
	}

	var location string
	if err := chromedp.Run(browserCtx, chromedp.Location(&location)); err != nil {
		t.Fatalf("Location: %v", err)
	}
	if location != interactiveBootstrapURL {
		t.Fatalf("got bootstrap URL %q, want %q", location, interactiveBootstrapURL)
	}
}

func TestExtractAfterInteraction_BlockedManualNavigationFailsClosed(t *testing.T) {
	w := newIntegrationWorker(t, Config{})
	requireInteractiveBrowser(t, w)

	res, browserCtx, cancelBrowser, cleanup, policy := w.startInteractive(context.Background(), models.SessionRequest{})
	if res != nil {
		t.Fatalf("expected blank interactive startup to succeed, got %+v", res)
	}
	defer cancelBrowser()
	defer cleanup()

	_ = chromedp.Run(browserCtx, chromedp.Navigate("https://127.0.0.1/"))
	violation := waitForPolicyViolation(t, policy)
	if violation.Code != models.CodeBlockedDestination {
		t.Fatalf("got violation code %q, want %q", violation.Code, models.CodeBlockedDestination)
	}

	extractRes := w.extractAfterInteraction(browserCtx, models.SessionRequest{}, policy)
	if extractRes.Error == nil {
		t.Fatalf("expected blocked manual navigation to fail closed, got %+v", extractRes)
	}
	if extractRes.Error.Code != models.CodeBlockedDestination {
		t.Fatalf("got extraction error %q, want %q", extractRes.Error.Code, models.CodeBlockedDestination)
	}
}

func TestExtractAfterInteraction_ManualPublicNavigationWorks(t *testing.T) {
	targetURL := requireInteractivePublicURL(t)
	parsedTarget, err := url.Parse(targetURL)
	if err != nil {
		t.Fatalf("parse CHROME_CONTROL_IT_URL: %v", err)
	}

	w := newIntegrationWorker(t, Config{})
	requireInteractiveBrowser(t, w)
	res, browserCtx, cancelBrowser, cleanup, policy := w.startInteractive(context.Background(), models.SessionRequest{
		MaxTextChars: 2048,
	})
	if res != nil {
		t.Fatalf("expected blank interactive startup to succeed, got %+v", res)
	}
	defer cancelBrowser()
	defer cleanup()

	if err := chromedp.Run(browserCtx, chromedp.Navigate(targetURL)); err != nil {
		t.Fatalf("manual Navigate(%q): %v", targetURL, err)
	}

	extractRes := w.extractAfterInteraction(browserCtx, models.SessionRequest{
		MaxTextChars: 2048,
	}, policy)
	if extractRes.Error != nil {
		t.Fatalf("expected extraction after manual navigation to succeed, got %+v", extractRes)
	}
	if extractRes.FinalURL == "" {
		t.Fatal("expected final_url after manual navigation")
	}
	finalURL, err := url.Parse(extractRes.FinalURL)
	if err != nil {
		t.Fatalf("parse final_url %q: %v", extractRes.FinalURL, err)
	}
	if finalURL.Hostname() != parsedTarget.Hostname() {
		t.Fatalf("got final_url host %q, want %q", finalURL.Hostname(), parsedTarget.Hostname())
	}
}

func TestStartInteractive_SuppliedURLStillValidates(t *testing.T) {
	w := New(Config{})
	res, browserCtx, cancelBrowser, cleanup, policy := w.startInteractive(context.Background(), models.SessionRequest{
		URL: "https://127.0.0.1/",
	})
	defer cancelBrowser()
	if cleanup != nil {
		defer cleanup()
	}

	if res == nil || res.Error == nil {
		t.Fatal("expected blocked supplied URL to fail validation")
	}
	if res.Error.Code != models.CodeBlockedDestination {
		t.Fatalf("got error code %q, want %q", res.Error.Code, models.CodeBlockedDestination)
	}
	if browserCtx != nil {
		t.Fatal("expected no browser context on validation failure")
	}
	if policy != nil {
		t.Fatal("expected no policy state on validation failure")
	}
}
