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
- Extract up to 100 visible links
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

## Threat Model

The primary threat is **Server-Side Request Forgery (SSRF)**: the worker fetches attacker-controlled URLs and may be directed to reach internal services, cloud metadata endpoints, or other private infrastructure.

Secondary threats:

- **DNS rebinding**: a hostname resolves to a public IP during validation but to a private IP when Chromium resolves it.
- **Redirect-chain escape**: a redirect chain leads from a valid public page to a blocked destination.
- **Subresource escape**: a public page loads frames, scripts, images, fetch requests, or WebSockets from private addresses.
- **Path traversal via task_id**: a client-supplied identifier used directly in a filesystem path enables directory traversal.
- **Untrusted content processing**: page content, titles, and extracted text are attacker-controlled and must never be interpreted as instructions.

Application-level URL validation is **defense in depth**. It does not eliminate SSRF by itself. Production deployments must apply independent network-level egress controls as the final security boundary.

---

## Architecture and Component Responsibilities

```text
chrome-control/
├── cmd/
│   └── worker/
│       └── main.go          # HTTP server, signal handling, graceful shutdown
├── internal/
│   ├── browser/
│   │   ├── worker.go        # Task orchestration, timeout, process-tree cleanup
│   │   └── extract.go       # Title, text, link, screenshot extraction
│   ├── security/
│   │   └── url_validator.go # URL and IP validation, DNS resolution
│   ├── models/
│   │   └── models.go        # Request/response/error types
│   └── artifacts/
│       └── store.go         # Artifact storage with opaque IDs and cleanup
├── tests/
│   ├── unit/
│   │   └── url_validator_test.go
│   └── integration/         # Build-tagged; requires Chromium
│       └── browser_test.go
├── go.mod
├── go.sum
├── Dockerfile
└── PLAN.md
```

**`url_validator`** owns all destination-policy decisions. No other component makes allow/deny decisions about network destinations.

**`worker`** owns the Chromium lifecycle. It must terminate the entire Chromium process tree on task completion, timeout, cancellation, or service shutdown.

**`store`** owns artifact lifecycle. It generates opaque random IDs and is the only component that maps IDs to filesystem paths.

---

## API

### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/tasks` | Execute one browser task |
| `GET` | `/healthz` | Process liveness check |
| `GET` | `/readyz` | Readiness check (can Chromium start?) |

`POST /v1/tasks` requirements:

- Accepts `Content-Type: application/json` only; reject other content types with `415`.
- Enforces a maximum request body of 64 KB.
- Rejects unknown JSON fields with `400`.
- Validates all fields before starting Chromium.
- Cancels task execution when the client disconnects.
- Rejects new tasks with `503` when the concurrent-task limit is reached.

Authentication is not implemented in the worker itself. It is enforced at the deployment boundary (reverse proxy or API gateway).

### Request

```json
{
  "task_id": "task_123",
  "url": "https://example.com",
  "capture_screenshot": true,
  "max_text_chars": 20000
}
```

Field constraints:

- `task_id`: required; 1–128 characters; must never be used directly in a filesystem path.
- `url`: required; must satisfy the complete destination policy defined below.
- `capture_screenshot`: optional; defaults to `false`.
- `max_text_chars`: optional; defaults to `20000` when omitted or `0`; clamped to the server maximum of `20000`.

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
  "screenshot_artifact_id": "artifact_7f3a9b2c",
  "error": null
}
```

### Error Model

All errors use a machine-readable code:

```go
type BrowserError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
```

| Code | HTTP status | Meaning |
|------|-------------|---------|
| `invalid_request` | 400 | Malformed JSON, unknown fields, constraint violation |
| `invalid_url` | 400 | URL fails scheme, credential, or format check |
| `blocked_destination` | 400 | Destination resolves to a blocked network |
| `dns_failure` | 400 | DNS resolution returned no usable addresses |
| `redirect_blocked` | 400 | A redirect target is a blocked destination |
| `redirect_limit_exceeded` | 400 | Redirect chain exceeded the 10-redirect limit |
| `browser_start_failed` | 500 | Chromium process did not start in time |
| `navigation_timeout` | 504 | Navigation did not complete within the timeout |
| `extraction_failed` | 500 | Page extraction raised an unexpected error |
| `screenshot_failed` | 500 | Screenshot capture or storage failed |
| `task_timeout` | 504 | Total task timeout exceeded |
| `overloaded` | 503 | Concurrent-task limit reached |

Security-policy failures (`blocked_destination`, `redirect_blocked`, `redirect_limit_exceeded`, `dns_failure`) fail closed: the worker must not return partial content or screenshots after a blocked request.

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
	Error                *BrowserError `json:"error,omitempty"`
}

type BrowserError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
```

---

## Security Invariants

1. Only absolute `https://` URLs with no embedded credentials are accepted.
2. The destination port must be `443` for MVP.
3. Hostnames are normalized before validation: trailing dots are removed and internationalized domain names are converted to their ASCII form.
4. IPv4-mapped IPv6 addresses (e.g. `::ffff:10.0.0.1`) are normalized with `net/netip`'s `Unmap` before classification.
5. DNS resolution must return at least one address; the request is rejected if **any** resolved address is blocked.
6. The same destination policy applies to initial navigation, every redirect, every frame, every subresource, workers, fetch/XHR requests, and WebSockets.
7. Every redirect destination is validated before Chromium is allowed to follow it. Chrome DevTools Protocol request interception is used to inspect and approve each hop.
8. A task may follow at most 10 redirects.
9. One Chromium process and one temporary profile are used per task.
10. Chromium runs non-root with its sandbox enabled. `--no-sandbox` is **prohibited**.
11. On task completion, cancellation, timeout, or service shutdown, the entire Chromium process tree must be terminated.
12. Temporary profiles and partial artifacts are removed on success, failure, timeout, cancellation, and service shutdown.
13. Page content is untrusted and must never be interpreted as worker instructions.
14. `task_id` is never used directly in a filesystem path.
15. Artifact IDs are opaque random identifiers; the artifact store is the only component that resolves them to filesystem paths.

Browser command-line flags and pre-navigation DNS checks are **not** considered security boundaries.

### Blocked Destinations

Reject any destination whose resolved IP falls in any of the following ranges, using `net/netip` for all parsing and checks:

- Loopback: `127.0.0.0/8`, `::1/128`
- Private-use: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`
- Link-local: `169.254.0.0/16`, `fe80::/10`
- Unspecified: `0.0.0.0/8`, `::/128`
- Multicast: `224.0.0.0/4`, `ff00::/8`
- Carrier-grade NAT: `100.64.0.0/10`
- Documentation / benchmarking / reserved: `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, `198.18.0.0/15`, `240.0.0.0/4`
- IPv4-mapped forms of any of the above (normalized with `Unmap` before classification)
- Known cloud metadata addresses: `169.254.169.254` (AWS/GCP/Azure), `fd00:ec2::254` (AWS IPv6)
- Hostnames `localhost` and any name under `.localhost`
- Hostname `metadata.google.internal`

If DNS resolution returns multiple addresses, the request is rejected if **any** of them is blocked.

### Network Enforcement

Application-level URL validation is defense in depth. The production deployment must additionally apply egress firewall or proxy rules so that Chromium cannot reach blocked networks even if application-level validation is bypassed.

DNS resolution and request interception alone do not fully prevent DNS rebinding unless browser traffic is either pinned to validated addresses or passes through a policy-enforcing egress proxy.

---

## Execution Lifecycle

```text
Receive POST /v1/tasks
  -> Validate Content-Type, body size, JSON fields
  -> Validate URL (scheme, credentials, port, hostname normalization)
  -> Resolve DNS; reject if any address is blocked
  -> Acquire concurrency slot; return 503 if limit reached
  -> Create temporary Chromium profile directory
  -> Launch isolated headless Chromium (non-root, sandbox enabled)
  -> Enable CDP request interception for all resource types
  -> Navigate to page; validate each redirect before following
  -> Wait for <body> to be ready (WaitReady); do not wait for network idle
  -> Extract title, visible text, visible links
  -> Optionally capture screenshot
  -> Store artifact with opaque random ID
  -> Return structured result
  -> Cancel CDP context; terminate Chromium process tree
  -> Delete temporary profile directory and any partial artifacts
  -> Release concurrency slot
```

The total task timeout covers all steps from DNS resolution through cleanup.

---

## Redirect Handling

Chromium is configured with CDP request interception enabled for all navigation and subresource requests. For each intercepted request:

1. Extract the destination URL.
2. Run the full destination-policy check (scheme, port, DNS resolution, IP classification).
3. If the destination is blocked, abort the request and return `redirect_blocked`.
4. If the redirect count has reached 10, abort and return `redirect_limit_exceeded`.
5. Otherwise, allow the request to continue.

This applies to top-level navigation redirects, frame navigation redirects, and any fetch/XHR redirect.

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
	// NOTE: --no-sandbox must never be added.

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancelBrowser := chromedp.NewContext(allocCtx)

	return ctx, func() {
		cancelBrowser()
		cancelAlloc()
		// Caller must also terminate the entire process tree.
	}
}
```

---

## Page Extraction

For this MVP, **visible text** means `document.body.innerText`. Text limits are measured in Unicode code points; truncation must not produce invalid UTF-8.

A **visible link** must:

- Have an absolute `http` or `https` URL.
- Have non-zero dimensions per `getBoundingClientRect()`.
- Not have `display: none`, `visibility: hidden`, or `opacity: 0` in its computed style.

Links are normalized (resolved against the page's base URL), deduplicated while preserving document order, and then truncated to the 100-link limit.

```go
func extractPage(ctx context.Context, maxChars int) (
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
			})()
		`, &links),
	)
	// Truncate text to maxChars Unicode code points without splitting UTF-8.
	text = truncateToCodePoints(text, maxChars)
	return
}
```

---

## URL Validation

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
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("destination is not permitted")
	}
	port := u.Port()
	if port != "" && port != "443" {
		return errors.New("only port 443 is permitted for MVP")
	}
	return nil
}

// isBlockedAddr returns true if addr is a non-public address.
// addr must already be Unmap()-ed.
func isBlockedAddr(addr netip.Addr) bool {
	blocked := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("::/128"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("ff00::/8"),
		netip.MustParsePrefix("fd00:ec2::254/128"),
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
```

DNS resolution must return at least one address. All resolved addresses are checked; the request is rejected if any is blocked.

---

## Artifact Lifecycle

- Artifact IDs are generated with `crypto/rand` (e.g. 128-bit hex). They are opaque to callers.
- The artifact store is the only component that maps an ID to a filesystem path; no other component constructs artifact paths.
- Path-traversal prevention: the store validates that the resolved artifact path is under the artifact root before opening or writing any file.
- File permissions: artifact files are created with mode `0600`; the artifact directory uses `0700`.
- Size limits: PNG screenshots are rejected if they exceed 10 MB on disk.
- Dimension limits: screenshots are captured at a maximum viewport of 1920×1080.
- Retention: artifacts are deleted immediately after the task response is sent. Configurable retention for debugging may be added in a future milestone.
- Cleanup on partial failure: any artifacts created during a task that ends in error or timeout are deleted before the response is returned.
- Retrieval authorization is enforced at the deployment boundary. The worker itself does not implement per-artifact authentication.

---

## Limits

| Limit | Value |
|---|---:|
| Total task timeout | 30 seconds |
| DNS timeout | 3 seconds |
| Chromium startup timeout | 5 seconds |
| Navigation timeout | 20 seconds |
| Extraction timeout | 5 seconds |
| Browser tabs | 1 |
| Redirects | 10 |
| Extracted text | 20,000 Unicode code points |
| Returned links | 100 |
| Screenshots | 1 |
| Screenshot file size | 10 MB |
| Screenshot viewport | 1920×1080 |
| Browser memory | 512 MB |
| Concurrent tasks per worker | Configurable, bounded |
| Request body size | 64 KB |

The total task timeout includes DNS resolution, Chromium startup, navigation, extraction, screenshot capture, and cleanup. Cleanup is not exempt from the timeout.

Page readiness is determined by `WaitReady("body")`. The worker does not wait for network idle, as that can block indefinitely on pages with long-polling.

---

## Process and Container Isolation

- Chromium runs as a non-root user.
- The Chromium sandbox is enabled. `--no-sandbox` is prohibited.
- The container uses a read-only root filesystem. Writable mounts are limited to the temporary profile directory and the artifact directory.
- No host network interfaces, Unix sockets, service-account credentials, or cloud-provider metadata endpoints are accessible from within the container.
- Linux capabilities are restricted to the minimum required; `CAP_SYS_ADMIN` and `CAP_NET_ADMIN` are dropped.
- On task cancellation, timeout, or service shutdown, the worker terminates the entire Chromium process tree (process group), not only the root process.
- Service concurrency is bounded. When the limit is reached, new requests receive `503` immediately. There is no unbounded queue.
- Deployment resource limits (CPU, memory, PIDs) are configured in the container runtime or orchestrator.

---

## Observability and Operations

- Structured JSON logs for each task: task ID, URL (scheme and host only, no path), duration, status, error code.
- Prometheus metrics (or equivalent): request count, in-flight count, task duration histogram, error counts by code.
- `/healthz` returns `200 OK` when the process is alive.
- `/readyz` returns `200 OK` when the worker is able to start Chromium, and `503` during startup or shutdown.
- Graceful shutdown: stop accepting new tasks, wait for in-flight tasks to complete (up to a configurable drain timeout), then terminate.

---

## Main Dependencies

```bash
go mod init github.com/groovy-sky/chrome-control

go get github.com/chromedp/chromedp
go get github.com/chromedp/cdproto
```

Commit `go.mod` and `go.sum`. Dependency versions must be pinned. Version updates are applied through reviewed dependency-update pull requests, not by running unversioned `go get` commands.

---

## Implementation Milestones

### Milestone 1 — Bootstrap

- Create Go module `github.com/groovy-sky/chrome-control`.
- Add `chromedp` and `cdproto`.
- Define request, response, and error types.
- Add `/healthz` and `/readyz` endpoints.

### Milestone 2 — URL Security

- Implement `validateURL`: scheme, credentials, port, hostname normalization.
- Implement `isBlockedAddr` with the full blocked-range list using `net/netip`.
- Implement DNS resolution with timeout; reject if any resolved address is blocked.
- Add injectable resolver interface for unit testing.
- Add unit tests covering all blocked ranges.

### Milestone 3 — Chromium Worker

- Create temporary profile directories with `os.MkdirTemp`.
- Launch Chromium non-root with sandbox enabled through `chromedp`.
- Enable CDP request interception for all resource types.
- Validate and intercept every redirect.
- Enforce per-task timeout; terminate entire process tree on expiry or cancellation.
- Remove profile directory on completion, failure, or timeout.

### Milestone 4 — Page Extraction

- Navigate to a validated URL.
- Extract title, final URL, visible text (truncated to Unicode code points), and visible links.
- Deduplicate and limit links.
- Return structured JSON.

### Milestone 5 — Screenshots

- Capture optional PNG screenshots.
- Store in artifact directory with opaque random IDs.
- Enforce size and dimension limits.
- Delete after response is sent.

### Milestone 6 — Containerization

- Add Dockerfile with Chromium installed.
- Run as non-root user with sandbox enabled.
- Use read-only root filesystem; writable mounts for profile and artifact directories only.
- Add CPU, memory, and PID limits in deployment configuration.
- Document egress firewall requirements.

---

## Tests

### Unit Tests

Run without Chromium or external network access.

- Reject non-HTTPS schemes.
- Reject URLs with embedded credentials.
- Reject non-443 ports.
- Reject `localhost` and `.localhost` names.
- Reject each blocked IP range (loopback, private, link-local, unspecified, multicast, CGNAT, documentation, benchmarking, reserved, cloud metadata).
- Reject IPv4-mapped IPv6 forms of blocked IPv4 addresses.
- Accept a valid public HTTPS URL.
- Verify that text truncation at the Unicode code-point boundary never produces invalid UTF-8.
- Verify `max_text_chars` clamping.
- Verify link deduplication preserves document order.
- Verify redirect-count tracking rejects at the 11th redirect.

Unit tests inject a mock resolver; they do not depend on external DNS.

### Integration Tests

Run with Chromium installed; gated by the `integration` build tag or a separate CI job.

- Open a controlled HTTPS test server (not an arbitrary public website).
- Extract title and visible text.
- Extract visible links; verify hidden links are excluded.
- Capture a screenshot; verify file is created and within size limit.
- Verify that a redirect from a public URL to a private destination is blocked.
- Verify that a private-network iframe is blocked.
- Verify that a private-network image, script, and stylesheet are blocked.
- Verify that a fetch/XHR request to a private destination is blocked.
- Verify that a WebSocket connection to a private destination is blocked.
- Verify that IPv4-mapped IPv6 forms of blocked addresses are blocked.
- Verify timeout behavior: task exceeding 30 seconds is terminated.
- Verify profile directory is removed after success, failure, and timeout.
- Verify artifact is removed after the response is sent.
- Verify the Chromium process tree is fully terminated after each task.
- Verify cleanup after Chromium startup failure.
- Verify cleanup after client cancellation.

---

## Definition of Done

- [ ] Worker accepts a URL request via `POST /v1/tasks`.
- [ ] Worker opens only validated public HTTPS URLs on port 443.
- [ ] Worker launches Chromium with a new temporary profile per task.
- [ ] Chromium runs non-root with sandbox enabled; `--no-sandbox` is absent.
- [ ] Worker returns title, final URL, visible text (Unicode-safe), and visible deduplicated links.
- [ ] Worker can optionally create a screenshot artifact with an opaque random ID.
- [ ] Worker blocks `localhost`, private networks, link-local, metadata, and all other non-public destinations.
- [ ] Redirects and every browser-initiated network request are validated against the destination policy.
- [ ] Deployment-level egress policy independently blocks unsafe networks.
- [ ] Worker enforces time and output limits as defined in the Limits table.
- [ ] Entire Chromium process tree is terminated after each task.
- [ ] Temporary profile and partial artifacts are removed after each task, including on failure and timeout.
- [ ] Artifact identifiers are opaque and cannot be used for path traversal.
- [ ] Concurrency is bounded; overload returns `503` predictably.
- [ ] Machine-readable error codes are returned for all failure modes.
- [ ] `task_id` is never used directly in a filesystem path.
- [ ] Unit tests pass without Chromium or external network.
- [ ] Integration tests pass with Chromium installed.
- [ ] `go.mod` and `go.sum` are committed with pinned dependency versions.
- [ ] Worker runs in a non-root container with the Chromium sandbox enabled.
