// Package browser owns the Chromium lifecycle for a single task: profile
// creation, launch, request interception, extraction and process-tree cleanup.
package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/groovy-sky/chrome-control/internal/artifacts"
	"github.com/groovy-sky/chrome-control/internal/models"
	"github.com/groovy-sky/chrome-control/internal/security"
)

// Worker limits.
const (
	DefaultTaskTimeout         = 30 * time.Second
	DefaultBrowserStartTimeout = 5 * time.Second
	DefaultNavigationTimeout   = 20 * time.Second
	DefaultExtractionTimeout   = 5 * time.Second

	// MaxRedirects is the maximum number of redirect hops a task may follow.
	MaxRedirects = 10

	ScreenshotWidth  = 1920
	ScreenshotHeight = 1080

	// MaxTaskIDLength bounds the client-supplied task identifier. The value
	// is never used in a filesystem path.
	MaxTaskIDLength = 128
)

// nonNetworkSchemes are schemes that never leave the browser and therefore
// carry no destination-policy risk.
var nonNetworkSchemes = map[string]bool{
	"data":       true,
	"blob":       true,
	"about":      true,
	"chrome":     true,
	"devtools":   true,
	"filesystem": true,
}

// Config configures a Worker.
type Config struct {
	// ChromePath is the Chromium executable. Empty means auto-detect.
	ChromePath string
	// Headful launches Chromium with a visible window instead of headless mode.
	// Requires DISPLAY to be set. Intended for local debugging only.
	Headful bool
	// DebugHold, when positive, keeps the Chromium window open for this
	// duration after extraction (and screenshot, if requested) completes.
	// The hold is skipped for readiness probes. Intended for local debugging.
	DebugHold time.Duration
	// Artifacts stores screenshots. Required for capture_screenshot.
	Artifacts *artifacts.Store
	// Resolver performs destination DNS lookups.
	Resolver security.Resolver
	// Logger receives structured task logs.
	Logger *slog.Logger

	TaskTimeout         time.Duration
	BrowserStartTimeout time.Duration
	NavigationTimeout   time.Duration
	ExtractionTimeout   time.Duration
}

func (c *Config) withDefaults() {
	if c.Resolver == nil {
		c.Resolver = security.NewDefaultResolver()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.TaskTimeout <= 0 {
		c.TaskTimeout = DefaultTaskTimeout
	}
	if c.BrowserStartTimeout <= 0 {
		c.BrowserStartTimeout = DefaultBrowserStartTimeout
	}
	if c.NavigationTimeout <= 0 {
		c.NavigationTimeout = DefaultNavigationTimeout
	}
	if c.ExtractionTimeout <= 0 {
		c.ExtractionTimeout = DefaultExtractionTimeout
	}
}

// Worker executes browser tasks, one isolated Chromium process per task.
type Worker struct {
	cfg Config
}

// New returns a Worker with defaults applied.
func New(cfg Config) *Worker {
	cfg.withDefaults()
	return &Worker{cfg: cfg}
}

// RedirectTracker enforces the per-task redirect limit.
type RedirectTracker struct {
	mu    sync.Mutex
	limit int
	count int
}

// NewRedirectTracker returns a tracker allowing at most limit redirect hops.
func NewRedirectTracker(limit int) *RedirectTracker {
	return &RedirectTracker{limit: limit}
}

// Record accounts for one redirect hop. It returns an error once the redirect
// limit has been exceeded, i.e. on hop limit+1.
func (t *RedirectTracker) Record() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.count++
	if t.count > t.limit {
		return &security.Error{
			Code:    models.CodeRedirectLimitExceeded,
			Message: fmt.Sprintf("redirect chain exceeded the %d-redirect limit", t.limit),
		}
	}
	return nil
}

// Count returns the number of redirect hops recorded so far.
func (t *RedirectTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
}

// policyState records the first destination-policy violation seen during a
// task. A task that records a violation fails closed.
type policyState struct {
	mu  sync.Mutex
	err *models.BrowserError
}

func (p *policyState) fail(err *models.BrowserError) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
}

func (p *policyState) violation() *models.BrowserError {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// ValidateRequest checks the request field constraints that do not require
// network access.
func ValidateRequest(req *models.BrowserRequest) *models.BrowserError {
	if l := len(req.TaskID); l == 0 || l > MaxTaskIDLength {
		return models.NewError(models.CodeInvalidRequest, "task_id must be 1-128 characters")
	}
	if req.URL == "" {
		return models.NewError(models.CodeInvalidRequest, "url is required")
	}
	if req.MaxTextChars < 0 {
		return models.NewError(models.CodeInvalidRequest, "max_text_chars must not be negative")
	}
	return nil
}

// Run executes one browser task and always returns a result. The returned
// result carries a machine-readable error when the task did not complete.
func (w *Worker) Run(ctx context.Context, req models.BrowserRequest) *models.BrowserResult {
	started := time.Now()
	result := w.run(ctx, req)
	logHost := ""
	if u, err := url.Parse(req.URL); err == nil {
		logHost = u.Scheme + "://" + u.Host
	}
	attrs := []any{
		slog.String("task_id", req.TaskID),
		slog.String("destination", logHost),
		slog.Duration("duration", time.Since(started)),
		slog.String("status", result.Status),
	}
	if result.Error != nil {
		attrs = append(attrs, slog.String("error_code", result.Error.Code))
	}
	w.cfg.Logger.Info("browser task finished", attrs...)
	return result
}

func (w *Worker) run(ctx context.Context, req models.BrowserRequest) *models.BrowserResult {
	fail := func(err *models.BrowserError) *models.BrowserResult {
		return &models.BrowserResult{TaskID: req.TaskID, Status: models.StatusFailed, Error: err}
	}

	if berr := ValidateRequest(&req); berr != nil {
		return fail(berr)
	}

	taskCtx, cancelTask := context.WithTimeout(ctx, w.cfg.TaskTimeout)
	defer cancelTask()

	// Destination policy for the initial navigation.
	if err := security.ValidateURLContext(taskCtx, req.URL, w.cfg.Resolver); err != nil {
		var perr *security.Error
		if errors.As(err, &perr) {
			return fail(perr.BrowserError())
		}
		return fail(models.NewError(models.CodeInvalidURL, "destination could not be validated"))
	}

	profileDir, err := os.MkdirTemp("", "chrome-control-profile-")
	if err != nil {
		return fail(models.NewError(models.CodeBrowserStartFailed, "could not create browser profile directory"))
	}
	defer os.RemoveAll(profileDir)

	browserCtx, cancelBrowser := w.createBrowserContext(taskCtx, profileDir)
	// Cleanup: cancel CDP contexts and terminate the whole process tree.
	var chromeProc *os.Process
	defer func() {
		cancelBrowser()
		killProcessTree(chromeProc)
	}()

	// Start Chromium within the startup budget. We run chromedp.Run on
	// browserCtx (not a child of it) so that the resulting browser target
	// remains valid for the rest of the task. The timeout is enforced by
	// racing the blocking call against a timer; on timeout we cancel the
	// browser context to trigger cleanup.
	startDone := make(chan error, 1)
	go func() { startDone <- chromedp.Run(browserCtx) }()

	startTimer := time.NewTimer(w.cfg.BrowserStartTimeout)
	defer startTimer.Stop()

	select {
	case err = <-startDone:
		if err != nil {
			w.cfg.Logger.Debug("chromium startup error", slog.String("error", err.Error()))
			return fail(models.NewError(models.CodeBrowserStartFailed, "chromium did not start"))
		}
	case <-startTimer.C:
		cancelBrowser()
		return fail(models.NewError(models.CodeBrowserStartFailed, "chromium startup timed out"))
	case <-taskCtx.Done():
		cancelBrowser()
		if errors.Is(taskCtx.Err(), context.Canceled) {
			return fail(models.NewError(models.CodeNavigationTimeout, "task cancelled by client"))
		}
		return fail(models.NewError(models.CodeTaskTimeout, "task timeout exceeded"))
	}
	if c := chromedp.FromContext(browserCtx); c != nil && c.Browser != nil {
		chromeProc = c.Browser.Process()
	}

	policy := &policyState{}
	redirects := NewRedirectTracker(MaxRedirects)
	w.interceptRequests(browserCtx, policy, redirects)

	if err := chromedp.Run(browserCtx, fetch.Enable().WithPatterns([]*fetch.RequestPattern{
		{URLPattern: "*", RequestStage: fetch.RequestStageRequest},
	})); err != nil {
		w.cfg.Logger.Debug("could not enable request interception", slog.String("error", err.Error()))
		return fail(models.NewError(models.CodeBrowserStartFailed, "could not enable request interception"))
	}

	navCtx, cancelNav := context.WithTimeout(browserCtx, w.cfg.NavigationTimeout)
	navErr := chromedp.Run(navCtx, chromedp.Navigate(req.URL))
	cancelNav()

	// Fail closed on any destination-policy violation, even if navigation
	// itself reported success.
	if v := policy.violation(); v != nil {
		return fail(v)
	}
	if navErr != nil {
		return fail(w.navigationError(taskCtx, navErr))
	}

	extractCtx, cancelExtract := context.WithTimeout(browserCtx, w.cfg.ExtractionTimeout)
	title, finalURL, text, links, extractErr := extractPage(extractCtx, req.MaxTextChars)
	cancelExtract()
	if v := policy.violation(); v != nil {
		return fail(v)
	}
	if extractErr != nil {
		if taskCtx.Err() != nil {
			return fail(models.NewError(models.CodeTaskTimeout, "task timeout exceeded"))
		}
		return fail(models.NewError(models.CodeExtractionFailed, "page extraction failed"))
	}

	result := &models.BrowserResult{
		TaskID:      req.TaskID,
		Status:      models.StatusCompleted,
		FinalURL:    finalURL,
		Title:       title,
		VisibleText: text,
		Links:       links,
	}

	if req.CaptureScreenshot {
		id, berr := w.screenshot(browserCtx)
		if berr != nil {
			return fail(berr)
		}
		// A violation observed while rendering invalidates the artifact.
		if v := policy.violation(); v != nil {
			if w.cfg.Artifacts != nil {
				_ = w.cfg.Artifacts.Delete(id)
			}
			return fail(v)
		}
		result.ScreenshotArtifactID = id
	}

	// Debug hold: keep Chromium open for inspection before cleanup.
	if w.cfg.DebugHold > 0 {
		select {
		case <-time.After(w.cfg.DebugHold):
		case <-taskCtx.Done():
		case <-ctx.Done():
		}
	}

	return result
}

// screenshot captures and stores a PNG screenshot, returning its artifact ID.
func (w *Worker) screenshot(ctx context.Context) (string, *models.BrowserError) {
	if w.cfg.Artifacts == nil {
		return "", models.NewError(models.CodeScreenshotFailed, "artifact storage is not configured")
	}
	shotCtx, cancel := context.WithTimeout(ctx, w.cfg.ExtractionTimeout)
	defer cancel()

	data, err := captureScreenshot(shotCtx)
	if err != nil {
		return "", models.NewError(models.CodeScreenshotFailed, "screenshot capture failed")
	}
	id, err := w.cfg.Artifacts.Save(data)
	if err != nil {
		return "", models.NewError(models.CodeScreenshotFailed, "screenshot could not be stored")
	}
	return id, nil
}

// navigationError classifies a navigation failure.
func (w *Worker) navigationError(taskCtx context.Context, err error) *models.BrowserError {
	if taskCtx.Err() != nil {
		if errors.Is(taskCtx.Err(), context.Canceled) {
			// Client disconnected (request context cancelled). Use
			// CodeNavigationTimeout so callers receive 504, not 500.
			return models.NewError(models.CodeNavigationTimeout, "task cancelled by client")
		}
		return models.NewError(models.CodeTaskTimeout, "task timeout exceeded")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return models.NewError(models.CodeNavigationTimeout, "navigation timed out")
	}
	if strings.Contains(err.Error(), "ERR_BLOCKED_BY_CLIENT") {
		return models.NewError(models.CodeBlockedDestination, "destination is not permitted")
	}
	return models.NewError(models.CodeNavigationTimeout, "navigation failed")
}

// createBrowserContext launches an isolated Chromium with a dedicated
// temporary profile. --no-sandbox is never added: the sandbox stays enabled
// and the process must run as a non-root user.
func (w *Worker) createBrowserContext(parent context.Context, profileDir string) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("headless", !w.cfg.Headful),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		// Explicitly keep the Chromium sandbox enabled; chromedp would
		// otherwise add --no-sandbox when running as root.
		chromedp.Flag("no-sandbox", false),
		chromedp.Flag("js-flags", "--max-old-space-size=512"),
		chromedp.ModifyCmdFunc(func(cmd *exec.Cmd) {
			setProcessGroup(cmd)
		}),
	)
	if w.cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(w.cfg.ChromePath))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancelBrowser := chromedp.NewContext(allocCtx)

	return ctx, func() {
		cancelBrowser()
		cancelAlloc()
		// The caller additionally terminates the entire process tree.
	}
}

// interceptRequests validates every request Chromium attempts to make,
// including the initial navigation, redirects, frames, subresources, workers,
// fetch/XHR and WebSocket handshakes.
func (w *Worker) interceptRequests(ctx context.Context, policy *policyState, redirects *RedirectTracker) {
	chromedp.ListenTarget(ctx, func(ev any) {
		paused, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		go w.handlePausedRequest(ctx, paused, policy, redirects)
	})
}

func (w *Worker) handlePausedRequest(ctx context.Context, ev *fetch.EventRequestPaused, policy *policyState, redirects *RedirectTracker) {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Target == nil {
		return
	}
	execCtx := cdp.WithExecutor(ctx, c.Target)

	requestURL := ""
	if ev.Request != nil {
		requestURL = ev.Request.URL
	}

	if berr := w.checkInterceptedRequest(ctx, requestURL, ev.RedirectedRequestID != "", redirects); berr != nil {
		policy.fail(berr)
		if err := fetch.FailRequest(ev.RequestID, network.ErrorReasonBlockedByClient).Do(execCtx); err != nil {
			w.cfg.Logger.Debug("failed to abort blocked request", slog.String("error", err.Error()))
		}
		return
	}
	if err := fetch.ContinueRequest(ev.RequestID).Do(execCtx); err != nil {
		w.cfg.Logger.Debug("failed to continue request", slog.String("error", err.Error()))
	}
}

// checkInterceptedRequest applies the destination policy to a single
// intercepted request.
func (w *Worker) checkInterceptedRequest(ctx context.Context, rawURL string, isRedirect bool, redirects *RedirectTracker) *models.BrowserError {
	if isRedirect {
		if err := redirects.Record(); err != nil {
			var perr *security.Error
			if errors.As(err, &perr) {
				return perr.BrowserError()
			}
			return models.NewError(models.CodeRedirectLimitExceeded, "redirect limit exceeded")
		}
	}

	if scheme, _, found := strings.Cut(rawURL, ":"); found && nonNetworkSchemes[strings.ToLower(scheme)] {
		return nil
	}

	// WebSocket handshakes reach interception as wss:// URLs; treat them
	// under the same policy as https.
	checkURL := rawURL
	if strings.HasPrefix(strings.ToLower(rawURL), "wss://") {
		checkURL = "https://" + rawURL[len("wss://"):]
	}

	if err := security.ValidateURLContext(ctx, checkURL, w.cfg.Resolver); err != nil {
		var perr *security.Error
		if errors.As(err, &perr) {
			code := perr.Code
			if isRedirect && code != models.CodeRedirectLimitExceeded {
				code = models.CodeRedirectBlocked
			}
			return models.NewError(code, perr.Message)
		}
		return models.NewError(models.CodeBlockedDestination, "destination is not permitted")
	}
	return nil
}

// Probe verifies that Chromium can be started, for readiness checks.
func (w *Worker) Probe(ctx context.Context) error {
	profileDir, err := os.MkdirTemp("", "chrome-control-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profileDir)

	probeCtx, cancel := context.WithTimeout(ctx, w.cfg.BrowserStartTimeout)
	defer cancel()

	browserCtx, cancelBrowser := w.createBrowserContext(probeCtx, profileDir)
	var proc *os.Process
	defer func() {
		cancelBrowser()
		killProcessTree(proc)
	}()

	if err := chromedp.Run(browserCtx); err != nil {
		return err
	}
	if c := chromedp.FromContext(browserCtx); c != nil && c.Browser != nil {
		proc = c.Browser.Process()
	}
	return nil
}
