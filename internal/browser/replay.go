// Package browser – replay.go implements the flow record-and-replay runner.
//
// RunFlow reuses the same secure Chromium launch, temporary-profile,
// request-interception and process-tree-cleanup path as Run (see worker.go).
// It does not weaken the destination policy: every browser request,
// including subsequent in-flow navigations, is intercepted and validated
// exactly as it is for a plain task.
package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/groovy-sky/chrome-control/internal/flows"
	"github.com/groovy-sky/chrome-control/internal/models"
	"github.com/groovy-sky/chrome-control/internal/security"
)

// DefaultFlowTimeout bounds an entire flow run, mirroring DefaultTaskTimeout
// for plain tasks. It is intentionally longer than DefaultTaskTimeout because
// a flow can contain many sequential steps.
const DefaultFlowTimeout = 90 * time.Second

// selectOptionJS selects a <select> element's option by matching either its
// value or its visible text, then fires input/change events. It is a fixed
// script; the option to match is passed as a CDP call argument (never
// string-interpolated), so it cannot be used to inject arbitrary script.
const selectOptionJS = `
function(value) {
	if (!(this instanceof HTMLSelectElement)) { return false; }
	let matched = false;
	for (const opt of this.options) {
		if (opt.value === value || opt.text === value) {
			this.value = opt.value;
			matched = true;
			break;
		}
	}
	if (!matched) { return false; }
	this.dispatchEvent(new Event('input', { bubbles: true }));
	this.dispatchEvent(new Event('change', { bubbles: true }));
	return true;
}`

// RunFlow validates and replays req.Flow, returning an opaque run ID together
// with per-step diagnostics. It always returns a non-nil result.
func (w *Worker) RunFlow(ctx context.Context, req flows.RunRequest) *flows.RunResult {
	started := time.Now()
	runID, err := GenerateToken()
	if err != nil {
		runID = ""
	}
	result := w.runFlow(ctx, runID, req)
	attrs := []any{
		slog.String("run_id", result.RunID),
		slog.Duration("duration", time.Since(started)),
		slog.String("status", result.Status),
		slog.Int("step_count", len(req.Flow.Steps)),
	}
	if result.Error != nil {
		attrs = append(attrs, slog.String("error_code", result.Error.Code))
	}
	w.cfg.Logger.Info("flow run finished", attrs...)
	return result
}

func (w *Worker) runFlow(ctx context.Context, runID string, req flows.RunRequest) *flows.RunResult {
	fail := func(berr *models.BrowserError) *flows.RunResult {
		return &flows.RunResult{RunID: runID, Status: models.StatusFailed, Error: berr}
	}

	if berr := flows.Validate(&req.Flow); berr != nil {
		return fail(berr)
	}

	flowCtx, cancelFlow := context.WithTimeout(ctx, w.cfg.FlowTimeout)
	defer cancelFlow()

	// Destination policy for the initial navigation, checked before the
	// browser is even launched. Any later navigate step is validated by the
	// same request-interception path used for every other browser request.
	if initialURL := firstNavigationURL(req.Flow); initialURL != "" {
		if err := security.ValidateURLContext(flowCtx, initialURL, w.cfg.Resolver); err != nil {
			var perr *security.Error
			if errors.As(err, &perr) {
				return fail(perr.BrowserError())
			}
			return fail(models.NewError(models.CodeInvalidURL, "destination could not be validated"))
		}
	}

	profileDir, err := os.MkdirTemp("", "chrome-control-profile-")
	if err != nil {
		return fail(models.NewError(models.CodeBrowserStartFailed, "could not create browser profile directory"))
	}
	defer os.RemoveAll(profileDir)

	browserCtx, cancelBrowser := w.createBrowserContext(flowCtx, profileDir)
	var chromeProc *os.Process
	defer func() {
		cancelBrowser()
		killProcessTree(chromeProc)
	}()

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
	case <-flowCtx.Done():
		cancelBrowser()
		if errors.Is(flowCtx.Err(), context.Canceled) {
			return fail(models.NewError(models.CodeNavigationTimeout, "run cancelled by client"))
		}
		return fail(models.NewError(models.CodeTaskTimeout, "flow timeout exceeded"))
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

	result := &flows.RunResult{RunID: runID, Status: models.StatusCompleted}

	if req.Flow.StartURL != "" {
		stepRes := w.executeStep(browserCtx, flows.Step{ID: "start_url", Type: flows.StepNavigate, URL: req.Flow.StartURL}, policy)
		if stepRes.Status == flows.StepStatusFailed {
			result.Status = models.StatusFailed
			result.Error = stepRes.Error
			w.recordFinalURL(browserCtx, result)
			return result
		}
	}

	for _, step := range req.Flow.Steps {
		stepRes := w.executeStep(browserCtx, step, policy)
		result.Steps = append(result.Steps, stepRes)
		if stepRes.Status == flows.StepStatusFailed {
			result.Status = models.StatusFailed
			result.Error = stepRes.Error
			break
		}
	}

	if v := policy.violation(); v != nil {
		result.Status = models.StatusFailed
		result.Error = v
	}

	w.recordFinalURL(browserCtx, result)

	if result.Status == models.StatusCompleted && req.CaptureScreenshot {
		id, berr := w.screenshot(browserCtx)
		if berr != nil {
			result.Status = models.StatusFailed
			result.Error = berr
			return result
		}
		if v := policy.violation(); v != nil {
			if w.cfg.Artifacts != nil {
				_ = w.cfg.Artifacts.Delete(id)
			}
			result.Status = models.StatusFailed
			result.Error = v
			return result
		}
		result.FinalScreenshotArtifactID = id
	}

	return result
}

// firstNavigationURL returns the URL that will be navigated to first: the
// flow's start_url if set, otherwise the URL of a leading "navigate" step.
func firstNavigationURL(f flows.Flow) string {
	if f.StartURL != "" {
		return f.StartURL
	}
	if len(f.Steps) > 0 && f.Steps[0].Type == flows.StepNavigate {
		return f.Steps[0].URL
	}
	return ""
}

// recordFinalURL best-effort records the current page URL. Errors are
// ignored: the browser context may already be in a failed/torn-down state.
func (w *Worker) recordFinalURL(browserCtx context.Context, result *flows.RunResult) {
	var finalURL string
	locCtx, cancel := context.WithTimeout(browserCtx, w.cfg.ExtractionTimeout)
	defer cancel()
	if err := chromedp.Run(locCtx, chromedp.Location(&finalURL)); err == nil {
		result.FinalURL = finalURL
	}
}

// executeStep runs a single step with a bounded timeout and returns its
// result. The step timeout is a child of browserCtx (itself bounded by the
// overall flow deadline), so a step-specific timeout can only shorten, never
// extend, the run deadline.
func (w *Worker) executeStep(browserCtx context.Context, step flows.Step, policy *policyState) flows.StepResult {
	started := time.Now()
	res := flows.StepResult{ID: step.ID, Type: step.Type}

	timeoutMs := step.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = flows.DefaultStepTimeoutMs
	}
	stepCtx, cancel := context.WithTimeout(browserCtx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	var berr *models.BrowserError

	switch step.Type {
	case flows.StepNavigate:
		if err := chromedp.Run(stepCtx, chromedp.Navigate(step.URL)); err != nil {
			berr = classifyStepError(stepCtx, err)
		}
	case flows.StepClick:
		sel := cssSelector(step.Locator)
		if err := chromedp.Run(stepCtx,
			chromedp.WaitVisible(sel, chromedp.ByQuery),
			chromedp.Click(sel, chromedp.ByQuery),
		); err != nil {
			berr = classifyStepError(stepCtx, err)
		}
	case flows.StepFill:
		sel := cssSelector(step.Locator)
		if err := chromedp.Run(stepCtx,
			chromedp.WaitVisible(sel, chromedp.ByQuery),
			chromedp.Clear(sel, chromedp.ByQuery),
			chromedp.SendKeys(sel, step.Value, chromedp.ByQuery),
		); err != nil {
			berr = classifyStepError(stepCtx, err)
		}
	case flows.StepSelect:
		sel := cssSelector(step.Locator)
		if err := chromedp.Run(stepCtx,
			chromedp.WaitVisible(sel, chromedp.ByQuery),
			selectOption(sel, step.Value, chromedp.ByQuery),
		); err != nil {
			berr = classifyStepError(stepCtx, err)
		}
	case flows.StepWaitVisible, flows.StepAssertVisible:
		sel := cssSelector(step.Locator)
		if err := chromedp.Run(stepCtx, chromedp.WaitVisible(sel, chromedp.ByQuery)); err != nil {
			berr = classifyStepError(stepCtx, err)
		}
	case flows.StepAssertURL:
		var currentURL string
		if err := chromedp.Run(stepCtx, chromedp.Location(&currentURL)); err != nil {
			berr = classifyStepError(stepCtx, err)
		} else if currentURL != step.URL {
			// assert_url uses an exact-match comparison against the
			// current URL as reported by the browser (see README).
			berr = models.NewError(models.CodeFlowStepFailed, "assert_url: current URL did not match the expected URL")
		}
	case flows.StepScreenshot:
		id, screenshotErr := w.screenshot(stepCtx)
		if screenshotErr != nil {
			berr = screenshotErr
		} else {
			res.ScreenshotArtifactID = id
		}
	default:
		berr = models.NewError(models.CodeFlowStepFailed, "unsupported step type")
	}

	res.DurationMs = time.Since(started).Milliseconds()

	// A destination-policy violation observed by request interception during
	// this step overrides any other outcome: the run must fail closed.
	if v := policy.violation(); v != nil {
		res.Status = flows.StepStatusFailed
		res.Error = v
		return res
	}

	if berr != nil {
		res.Status = flows.StepStatusFailed
		res.Error = berr
		// Best-effort diagnostic screenshot on failure. The "screenshot" step
		// already attempted its own capture above, so it is not retried here.
		if step.Type != flows.StepScreenshot && w.cfg.Artifacts != nil {
			if id, serr := w.screenshot(browserCtx); serr == nil {
				res.ScreenshotArtifactID = id
			}
		}
		return res
	}

	res.Status = flows.StepStatusCompleted
	return res
}

// classifyStepError converts a chromedp/context error into a
// *models.BrowserError without leaking step field values (e.g. fill values)
// into the message.
func classifyStepError(ctx context.Context, err error) *models.BrowserError {
	var berr *models.BrowserError
	if errors.As(err, &berr) {
		return berr
	}
	if ctx.Err() != nil {
		return models.NewError(models.CodeFlowStepFailed, "step timed out before completing")
	}
	return models.NewError(models.CodeFlowStepFailed, "step execution failed")
}

// cssSelector converts a Locator into the CSS selector chromedp should query.
// "id" and "name" strategies are rendered as attribute selectors so that
// values never need CSS.escape()-style identifier escaping.
func cssSelector(loc *flows.Locator) string {
	if loc == nil {
		return ""
	}
	switch loc.Strategy {
	case flows.LocatorID:
		return `[id="` + escapeCSSAttrValue(loc.Value) + `"]`
	case flows.LocatorName:
		return `[name="` + escapeCSSAttrValue(loc.Value) + `"]`
	default: // flows.LocatorCSS, validated by flows.Validate.
		return loc.Value
	}
}

// escapeCSSAttrValue escapes backslashes and double quotes so that an
// attribute-selector value cannot break out of its quoted string.
func escapeCSSAttrValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// selectOption is a chromedp query action that selects a <select> element's
// option by value or visible text using a fixed script (selectOptionJS). The
// candidate value is passed as a CDP call argument, never string-interpolated
// into script source.
func selectOption(sel any, value string, opts ...chromedp.QueryOption) chromedp.QueryAction {
	return chromedp.QueryAfter(sel, func(ctx context.Context, execCtx runtime.ExecutionContextID, nodes ...*cdp.Node) error {
		if len(nodes) < 1 {
			return fmt.Errorf("locator did not match any nodes")
		}
		r, err := dom.ResolveNode().WithNodeID(nodes[0].NodeID).Do(ctx)
		if err != nil {
			return err
		}
		var matched bool
		err = chromedp.CallFunctionOn(selectOptionJS, &matched,
			func(p *runtime.CallFunctionOnParams) *runtime.CallFunctionOnParams {
				return p.WithObjectID(r.ObjectID)
			},
			value,
		).Do(ctx)
		// Best-effort release; the page may already have navigated away.
		_ = runtime.ReleaseObject(r.ObjectID).Do(ctx)
		if err != nil {
			return err
		}
		if !matched {
			return fmt.Errorf("target is not a <select> or has no matching option")
		}
		return nil
	}, opts...)
}
