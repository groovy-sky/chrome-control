# PLAN.md — MVP: Isolated Chromium Worker

## Goal

Build a small Go service that uses `github.com/chromedp/chromedp` to safely open a public web page and return:

- Final URL
- Page title
- Visible text
- Visible links
- Optional screenshot

This is a **read-only browser worker** for an LLM agent. The LLM may request a URL, but the worker validates and executes it.

---

## Scope

### Included

- Launch headless Chromium with `chromedp`
- Use a fresh temporary browser profile per task
- Open one HTTPS URL
- Extract page title and visible text
- Extract up to 100 links
- Capture an optional screenshot
- Enforce timeout and cleanup
- Block unsafe URLs and private-network access

### Not Included

- Search engine integration
- Clicking or form filling
- Login or credential handling
- File uploads or downloads
- Multiple tabs
- Persistent cookies or browser profiles
- Arbitrary JavaScript execution
- Browser extensions

---

## API

### Request

```json
{
  "task_id": "task_123",
  "url": "https://example.com",
  "capture_screenshot": true,
  "max_text_chars": 20000
}
```

### Response

```json
{
  "task_id": "task_123",
  "status": "completed",
  "final_url": "https://example.com/",
  "title": "Example Domain",
  "visible_text": "Example Domain ...",
  "links": [
    {
      "text": "More information",
      "url": "https://www.iana.org/domains/example"
    }
  ],
  "screenshot_artifact_id": "artifact_123",
  "error": null
}
```

---

## Security Rules

1. Allow only `https://` URLs.
2. Reject:
   - `http://`
   - `file://`
   - `data:`
   - `javascript:`
   - `chrome:`
   - `localhost`
   - loopback, private, link-local, and cloud-metadata IPs
3. Resolve the hostname before browser navigation and reject unsafe IPs.
4. Use one Chromium process and one temporary profile per task.
5. Delete the temporary profile after every task.
6. Set a hard task timeout of 30 seconds.
7. Limit extracted text to 20,000 characters.
8. Limit returned links to 100.
9. Do not expose cookies, storage, headers, browser paths, or secrets.
10. Treat all page content as untrusted external data.

---

## Project Structure

```text
browser-worker/
├── cmd/
│   └── worker/
│       └── main.go
├── internal/
│   ├── browser/
│   │   ├── worker.go
│   │   └── extract.go
│   ├── security/
│   │   └── url_validator.go
│   ├── models/
│   │   └── models.go
│   └── artifacts/
│       └── store.go
├── tests/
│   ├── browser_test.go
│   └── url_validator_test.go
├── go.mod
├── Dockerfile
└── PLAN.md
```

---

## Main Dependencies

```bash
go mod init github.com/example/browser-worker

go get github.com/chromedp/chromedp
go get github.com/chromedp/cdproto
```

---

## Core Flow

```text
Receive request
  ->
Validate URL and hostname
  ->
Resolve DNS and reject private IPs
  ->
Create temporary Chromium profile
  ->
Launch isolated headless Chromium
  ->
Navigate to page
  ->
Extract title, visible text, and links
  ->
Optionally capture screenshot
  ->
Return structured result
  ->
Close browser and delete temporary profile
```

---

## Core Types

```go
type BrowserRequest struct {
	TaskID            string `json:"task_id"`
	URL               string `json:"url"`
	CaptureScreenshot bool   `json:"capture_screenshot"`
	MaxTextChars      int    `json:"max_text_chars"`
}

type VisibleLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type BrowserResult struct {
	TaskID               string        `json:"task_id"`
	Status               string        `json:"status"`
	FinalURL             string        `json:"final_url,omitempty"`
	Title                string        `json:"title,omitempty"`
	VisibleText          string        `json:"visible_text,omitempty"`
	Links                []VisibleLink `json:"links,omitempty"`
	ScreenshotArtifactID string        `json:"screenshot_artifact_id,omitempty"`
	Error                string        `json:"error,omitempty"`
}
```

---

## Chromium Setup

```go
func createBrowserContext(
	parent context.Context,
	chromeBin string,
	profileDir string,
) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromeBin),
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancelBrowser := chromedp.NewContext(allocCtx)

	return ctx, func() {
		cancelBrowser()
		cancelAlloc()
	}
}
```

---

## Page Extraction

```go
func extractPage(ctx context.Context) (
	title string,
	currentURL string,
	text string,
	links []VisibleLink,
	err error,
) {
	err = chromedp.Run(ctx,
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Title(&title),
		chromedp.Location(&currentURL),
		chromedp.Evaluate(`
			(() => document.body?.innerText || "")()
		`, &text),
		chromedp.Evaluate(`
			(() => Array.from(document.querySelectorAll("a[href]"))
				.slice(0, 100)
				.map(a => ({
					text: (a.innerText || "").trim(),
					url: a.href
				})))()
		`, &links),
	)

	return
}
```

---

## URL Validation

The validator must reject:

```text
localhost
127.0.0.0/8
10.0.0.0/8
172.16.0.0/12
192.168.0.0/16
169.254.0.0/16
::1
fc00::/7
fe80::/10
169.254.169.254
metadata.google.internal
```

Basic validation rules:

```go
func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	if u.Scheme != "https" {
		return errors.New("only HTTPS URLs are allowed")
	}

	if u.User != nil {
		return errors.New("URLs with embedded credentials are not allowed")
	}

	if u.Hostname() == "" {
		return errors.New("URL host is required")
	}

	return nil
}
```

DNS validation must run before navigation and reject hosts resolving to blocked IP ranges.

---

## Limits

| Limit | Value |
|---|---:|
| Total task timeout | 30 seconds |
| Navigation timeout | 20 seconds |
| Browser tabs | 1 |
| Redirects | 10 |
| Extracted text | 20,000 characters |
| Returned links | 100 |
| Screenshots | 1 |
| Screenshot size | 10 MB |
| Browser memory | 512 MB |

---

## Implementation Tasks

### 1. Bootstrap

- Create Go module.
- Add `chromedp`.
- Define request and response structs.
- Add `/health` endpoint.

### 2. URL Security

- Validate URL scheme and hostname.
- Resolve DNS.
- Block private, loopback, and metadata IP ranges.
- Add unit tests.

### 3. Chromium Worker

- Create temporary profile directories.
- Launch Chromium through `chromedp`.
- Add task timeout.
- Ensure browser contexts are always canceled.
- Remove profiles after completion.

### 4. Page Extraction

- Navigate to a validated URL.
- Extract title, final URL, text, and links.
- Truncate text and links to configured limits.
- Return structured JSON.

### 5. Screenshots

- Capture optional PNG screenshots.
- Store screenshots in an artifact directory.
- Return an artifact ID instead of a filesystem path.

### 6. Containerization

- Add Dockerfile with Chromium installed.
- Run as a non-root user.
- Use temporary writable directories only for profiles and artifacts.
- Add CPU, memory, and process limits in deployment configuration.

---

## Tests

### Unit Tests

- Reject non-HTTPS schemes.
- Reject `localhost`.
- Reject private IPv4 and IPv6 addresses.
- Reject cloud metadata endpoints.
- Verify text truncation.
- Verify link limit.

### Integration Tests

- Open a public HTTPS test page.
- Extract title and visible text.
- Extract links.
- Capture a screenshot.
- Verify timeout behavior.
- Verify profile directory cleanup.

---

## Definition of Done

- [ ] Worker accepts a URL request.
- [ ] Worker opens only validated public HTTPS URLs.
- [ ] Worker launches Chromium with a new temporary profile per task.
- [ ] Worker returns title, final URL, visible text, and links.
- [ ] Worker can optionally create a screenshot artifact.
- [ ] Worker blocks localhost, private networks, and metadata endpoints.
- [ ] Worker enforces time and output limits.
- [ ] Browser process and temporary profile are removed after each task.
- [ ] Unit and integration tests pass.
- [ ] Worker runs in a non-root container.
