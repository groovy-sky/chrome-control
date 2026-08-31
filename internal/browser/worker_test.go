package browser

import (
	"context"
	"testing"

	"github.com/groovy-sky/chrome-control/internal/models"
)

func TestStartInteractive_SuppliedAboutBlankIsRejected(t *testing.T) {
	t.Parallel()

	w := New(Config{})
	res, browserCtx, cancelBrowser, cleanup, policy := w.startInteractive(context.Background(), models.SessionRequest{
		URL: interactiveBootstrapURL,
	})
	defer cancelBrowser()
	if cleanup != nil {
		defer cleanup()
	}

	if res == nil || res.Error == nil {
		t.Fatal("expected supplied about:blank URL to be rejected")
	}
	if res.Error.Code != models.CodeInvalidURL {
		t.Fatalf("expected %s, got %s", models.CodeInvalidURL, res.Error.Code)
	}
	if browserCtx != nil {
		t.Fatal("expected no browser context on validation failure")
	}
	if policy != nil {
		t.Fatal("expected no policy state on validation failure")
	}
}
