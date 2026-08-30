# chrome-control

An isolated Chromium browser worker for LLM agents. The worker safely opens
public web pages and returns the page title, visible text, extracted links, the
final URL, and an optional screenshot.

Two entry points are provided:

| Binary | Transport | Use case |
|--------|-----------|----------|
| `cmd/worker` | HTTP | Cloud / container deployments |
| `cmd/mcp`   | MCP stdio | Desktop / CLI MCP clients |

---

## Security model

Both entry points share the same hardened execution path:

- **URL validation** – only publicly routable HTTPS destinations are accepted.
  Private IP ranges, loopback, link-local, and non-HTTPS schemes are blocked
  before any network access.
- **Request interception** – every Chromium request (redirects, frames,
  sub-resources, WebSocket handshakes) is intercepted and validated.
- **Redirect limit** – tasks abort after more than 10 redirect hops.
- **Per-task Chromium isolation** – each task launches a fresh Chromium process
  with a dedicated temporary profile that is deleted on completion.
- **Sandbox** – the Chromium sandbox is always enabled; the binary must run as
  a non-root user.
- **Task timeout** – tasks are hard-cancelled after 30 seconds (configurable).
- **Concurrency limit** – the HTTP server rejects tasks that exceed the
  concurrent-task ceiling (default 4).

> **Deployment note:** Public HTTPS destinations only. Network-level egress
> controls (firewall / VPC egress policy) remain required; the worker's
> validation is defence-in-depth, not a substitute for network policy.

---

## HTTP worker

### Build and run

```sh
go build -o worker ./cmd/worker
./worker
```

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `ADDR` | `:8080` | Listen address |
| `ARTIFACT_DIR` | `$TMPDIR/chrome-control-artifacts` | Screenshot storage |
| `MAX_CONCURRENT_TASKS` | `4` | Concurrency ceiling |
| `CHROME_PATH` | *(auto-detect)* | Chromium executable path |

### Endpoints

```
POST /v1/tasks   – submit a browsing task
GET  /healthz    – liveness probe
GET  /readyz     – readiness probe
```

### Example request

```sh
curl -X POST http://localhost:8080/v1/tasks \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"t1","url":"https://example.com"}'
```

---

## MCP server (stdio)

The `cmd/mcp` binary implements the
[Model Context Protocol](https://modelcontextprotocol.io/) over stdio.
All operational logs are written to **stderr**; stdout carries only MCP
protocol messages.

### Build and run

```sh
go build -o chrome-control-mcp ./cmd/mcp
./chrome-control-mcp
```

The same environment variables as the HTTP worker apply (except `ADDR` and
`MAX_CONCURRENT_TASKS` which are HTTP-specific).

### Tool: `browse_url`

```json
{
  "name": "browse_url",
  "description": "Browse a public HTTPS URL using a headless Chromium instance.",
  "inputSchema": {
    "type": "object",
    "required": ["url"],
    "properties": {
      "url": {
        "type": "string",
        "description": "The URL to visit. Must be a publicly routable HTTPS URL."
      },
      "capture_screenshot": {
        "type": "boolean",
        "description": "When true, capture a PNG screenshot and return its artifact identifier."
      },
      "max_text_chars": {
        "type": "number",
        "description": "Maximum number of characters to include in visible_text. 0 means unlimited."
      },
      "task_id": {
        "type": "string",
        "description": "Optional opaque task identifier (1-128 characters). A random identifier is generated when omitted."
      }
    }
  }
}
```

#### Example tool call

```json
{
  "url": "https://example.com",
  "capture_screenshot": false,
  "max_text_chars": 2000
}
```

#### Successful response (content[0].text, JSON)

```json
{
  "task_id": "a3f1…",
  "status": "completed",
  "final_url": "https://example.com/",
  "title": "Example Domain",
  "visible_text": "Example Domain\nThis domain is for use in illustrative examples…",
  "links": [
    {"text": "More information...", "url": "https://www.iana.org/domains/reserved"}
  ]
}
```

#### Error response (content[0].text, JSON, `isError: true`)

```json
{
  "error": {
    "code": "blocked_destination",
    "message": "destination is not permitted"
  }
}
```

Error codes mirror those from the HTTP API:

| Code | Meaning |
|------|---------|
| `invalid_request` | Malformed arguments |
| `invalid_url` | URL is not a valid HTTPS URL |
| `blocked_destination` | Private/internal address |
| `dns_failure` | DNS resolution failed |
| `redirect_blocked` | A redirect pointed to a blocked destination |
| `redirect_limit_exceeded` | More than 10 redirect hops |
| `browser_start_failed` | Chromium could not be started |
| `navigation_timeout` | Page did not load within the timeout |
| `extraction_failed` | Could not extract page content |
| `screenshot_failed` | Screenshot capture or storage failed |
| `task_timeout` | Hard task timeout exceeded |
| `overloaded` | (HTTP only) concurrent-task limit reached |

### MCP client configuration (Claude Desktop example)

Add the following to your `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "chrome-control": {
      "command": "/path/to/chrome-control-mcp",
      "env": {
        "CHROME_PATH": "/usr/bin/chromium"
      }
    }
  }
}
```

Replace `/path/to/chrome-control-mcp` with the absolute path to the built
binary and adjust `CHROME_PATH` if Chromium is not on the default search path.

---

## Docker

The existing `Dockerfile` builds the HTTP worker (`cmd/worker`).  To add the
MCP binary to your own image:

```dockerfile
RUN go build -o /usr/local/bin/chrome-control-mcp ./cmd/mcp
```

---

## Development

```sh
# Format
gofmt -w .

# Test (no Chromium required)
go test ./...

# Build both binaries
go build ./cmd/worker ./cmd/mcp
```
