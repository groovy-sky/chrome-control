package browser

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/groovy-sky/chrome-control/internal/flows"
	"github.com/groovy-sky/chrome-control/internal/models"
)

// denyAllReplayResolver rejects every hostname resolution, used to exercise the
// destination-policy pre-check without ever launching a real Chromium binary.
type denyAllReplayResolver struct{}

func (denyAllReplayResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("resolution disabled for this test")
}

func TestCSSSelector(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		loc  *flows.Locator
		want string
	}{
		{"nil locator", nil, ""},
		{"css passthrough", &flows.Locator{Strategy: flows.LocatorCSS, Value: "#submit"}, "#submit"},
		{"id strategy", &flows.Locator{Strategy: flows.LocatorID, Value: "submit"}, `[id="submit"]`},
		{"name strategy", &flows.Locator{Strategy: flows.LocatorName, Value: "email"}, `[name="email"]`},
		{"id strategy escapes quotes", &flows.Locator{Strategy: flows.LocatorID, Value: `a"b`}, `[id="a\"b"]`},
		{"name strategy escapes backslash", &flows.Locator{Strategy: flows.LocatorName, Value: `a\b`}, `[name="a\\b"]`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cssSelector(tc.loc); got != tc.want {
				t.Fatalf("cssSelector(%+v) = %q, want %q", tc.loc, got, tc.want)
			}
		})
	}
}

func TestClassifyStepError_PassesThroughBrowserError(t *testing.T) {
	t.Parallel()
	original := models.NewError(models.CodeScreenshotFailed, "screenshot could not be stored")
	got := classifyStepError(context.Background(), original, nil)
	if got != original {
		t.Fatalf("expected original *BrowserError to pass through unchanged, got %+v", got)
	}
}

func TestClassifyStepError_TimeoutMapsToFlowStepFailed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	got := classifyStepError(ctx, errors.New("some chromedp failure"), nil)
	if got.Code != models.CodeFlowStepFailed {
		t.Fatalf("expected %s, got %s", models.CodeFlowStepFailed, got.Code)
	}
}

func TestClassifyStepError_GenericFailureMapsToFlowStepFailed(t *testing.T) {
	t.Parallel()
	got := classifyStepError(context.Background(), errors.New("some chromedp failure"), nil)
	if got.Code != models.CodeFlowStepFailed {
		t.Fatalf("expected %s, got %s", models.CodeFlowStepFailed, got.Code)
	}
}

func TestRunFlow_ValidationFailureNeverLaunchesBrowser(t *testing.T) {
	t.Parallel()
	w := New(Config{Resolver: denyAllReplayResolver{}})
	req := flows.RunRequest{Flow: flows.Flow{
		Version: flows.SupportedVersion,
		Name:    "bad",
		// No steps: fails validation before any browser/DNS interaction.
	}}
	result := w.RunFlow(context.Background(), req)
	if result == nil || result.Status != models.StatusFailed {
		t.Fatalf("expected failed result for invalid flow, got %+v", result)
	}
	if result.Error == nil || result.Error.Code != models.CodeInvalidRequest {
		t.Fatalf("expected invalid_request error, got %+v", result.Error)
	}
}

func TestRunFlow_BlockedStartURLFailsClosedBeforeLaunch(t *testing.T) {
	t.Parallel()
	w := New(Config{Resolver: denyAllReplayResolver{}})
	req := flows.RunRequest{Flow: flows.Flow{
		Version:  flows.SupportedVersion,
		Name:     "blocked",
		StartURL: "https://example.com/",
		Steps: []flows.Step{
			{ID: "s1", Type: flows.StepAssertURL, URL: "https://example.com/"},
		},
	}}
	result := w.RunFlow(context.Background(), req)
	if result == nil || result.Status != models.StatusFailed {
		t.Fatalf("expected failed result for unresolvable destination, got %+v", result)
	}
	if result.Error == nil || result.Error.Code != models.CodeDNSFailure {
		t.Fatalf("expected dns_failure error, got %+v", result.Error)
	}
	if len(result.Steps) != 0 {
		t.Fatalf("expected no steps to run before the browser launched, got %+v", result.Steps)
	}
}

func TestFirstNavigationURL(t *testing.T) {
	t.Parallel()

	f := flows.Flow{StartURL: "https://start.example.com/"}
	if got := firstNavigationURL(f); got != f.StartURL {
		t.Fatalf("expected start_url to take priority, got %q", got)
	}

	f2 := flows.Flow{Steps: []flows.Step{{Type: flows.StepNavigate, URL: "https://step.example.com/"}}}
	if got := firstNavigationURL(f2); got != "https://step.example.com/" {
		t.Fatalf("expected leading navigate step url, got %q", got)
	}

	f3 := flows.Flow{Steps: []flows.Step{{Type: flows.StepClick}}}
	if got := firstNavigationURL(f3); got != "" {
		t.Fatalf("expected empty string when there is no navigation, got %q", got)
	}
}
