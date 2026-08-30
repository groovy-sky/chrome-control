package browser

import (
	"context"

	"github.com/chromedp/chromedp"

	"github.com/groovy-sky/chrome-control/internal/models"
)

// Extraction limits.
const (
	// DefaultMaxTextChars is used when the request omits max_text_chars or sets it to 0.
	// It is intentionally equal to ServerMaxTextChars: the default is the maximum.
	// Changing either constant independently without updating the other will cause
	// clamping behaviour to diverge from the documented default, so both must stay in sync.
	DefaultMaxTextChars = 20000
	// ServerMaxTextChars is the server-side ceiling for max_text_chars.
	ServerMaxTextChars = 20000
	// MaxLinks is the maximum number of links returned for a page.
	MaxLinks = 100
)

// visibleTextJS returns the page's visible text.
const visibleTextJS = `(() => document.body?.innerText || "")()`

// visibleLinksJS collects absolute http(s) links that are rendered and
// visible, deduplicated in document order and limited to 100 entries.
const visibleLinksJS = `
(() => {
	const seen = new Set();
	return Array.from(document.querySelectorAll("a[href]"))
		.filter(a => {
			const u = a.href;
			if (!(u.startsWith("https://") || u.startsWith("http://"))) return false;
			if (seen.has(u)) return false;
			const rect = a.getBoundingClientRect();
			if (rect.width === 0 && rect.height === 0) return false;
			const style = getComputedStyle(a);
			if (style.display === "none" || style.visibility === "hidden" || style.opacity === "0") return false;
			seen.add(u);
			return true;
		})
		.slice(0, 100)
		.map(a => ({text: (a.innerText || "").trim(), url: a.href}));
})()`

// ClampMaxTextChars applies the max_text_chars defaulting and clamping rules:
// a missing, zero or negative value becomes the default, and any value above
// the server maximum is clamped down to it.
func ClampMaxTextChars(n int) int {
	if n <= 0 {
		return DefaultMaxTextChars
	}
	if n > ServerMaxTextChars {
		return ServerMaxTextChars
	}
	return n
}

// TruncateToCodePoints truncates s to at most n Unicode code points,
// never splitting a multi-byte sequence.
func TruncateToCodePoints(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// DedupeLinks removes duplicate URLs while preserving document order and
// truncates the result to the link limit. It mirrors the in-page filtering as
// defense in depth, since page content is untrusted.
func DedupeLinks(links []models.VisibleLink, limit int) []models.VisibleLink {
	if limit <= 0 {
		return nil
	}
	seen := make(map[string]bool, len(links))
	out := make([]models.VisibleLink, 0, len(links))
	for _, l := range links {
		if l.URL == "" || seen[l.URL] {
			continue
		}
		seen[l.URL] = true
		out = append(out, l)
		if len(out) == limit {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractPage waits for the document body and extracts the title, final URL,
// visible text and visible links from the current page.
func extractPage(ctx context.Context, maxChars int) (
	title string,
	currentURL string,
	text string,
	links []models.VisibleLink,
	err error,
) {
	err = chromedp.Run(ctx,
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Title(&title),
		chromedp.Location(&currentURL),
		chromedp.Evaluate(visibleTextJS, &text),
		chromedp.Evaluate(visibleLinksJS, &links),
	)
	if err != nil {
		return "", "", "", nil, err
	}
	text = TruncateToCodePoints(text, ClampMaxTextChars(maxChars))
	links = DedupeLinks(links, MaxLinks)
	return title, currentURL, text, links, nil
}

// captureScreenshot renders the current viewport as a PNG at a maximum
// viewport of 1920x1080.
func captureScreenshot(ctx context.Context) ([]byte, error) {
	var buf []byte
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(ScreenshotWidth, ScreenshotHeight),
		chromedp.CaptureScreenshot(&buf),
	)
	if err != nil {
		return nil, err
	}
	return buf, nil
}
