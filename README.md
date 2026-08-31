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
| `HEADFUL` | `false` | Launch Chromium with a visible window (**local debugging only**; requires `DISPLAY`) |
| `DEBUG_HOLD_SECONDS` | `0` | Seconds to keep the Chromium window open after a task completes (**local debugging only**) |
| `INTERACTIVE_TIMEOUT_SECONDS` | `300` | Maximum time (seconds) a session waits for `continue` before timing out |
| `MAX_INTERACTIVE_SESSIONS` | `2` | Maximum number of concurrent interactive sessions |

> ⚠️ **`HEADFUL` and `DEBUG_HOLD_SECONDS` are intended for local debugging only.**
> Do not enable them in production. `HEADFUL` requires an X server reachable
> via `DISPLAY`; `DEBUG_HOLD_SECONDS` delays task teardown and can exhaust
> concurrency slots under load.

### Endpoints

```
POST   /v1/tasks                       – submit a browsing task
POST   /v1/sessions                    – create an interactive session
GET    /v1/sessions/{token}            – get session status / result
POST   /v1/sessions/{token}/continue   – signal the session to extract and return
DELETE /v1/sessions/{token}            – cancel a session
GET    /healthz                        – liveness probe
GET    /readyz                         – readiness probe
```

### Example request

```sh
curl -X POST http://localhost:8080/v1/tasks \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"t1","url":"https://example.com"}'
```

---

## Interactive sessions (CAPTCHA / manual browsing)

Interactive sessions let you view and interact with the same Chromium instance
used by a task, for example to solve a CAPTCHA, before extraction runs.

### How it works

```
Host browser
    │
    │  http://127.0.0.1:6080/vnc.html   (noVNC image only)
    ▼
noVNC web client  →  VNC server  →  virtual X display  →  Chromium inside container
```

1. `POST /v1/sessions` – navigate to the URL and get a random opaque token.
2. Open the noVNC URL in your host browser and interact with the page.
3. `POST /v1/sessions/{token}/continue` – signal extraction to run.
4. `GET /v1/sessions/{token}` – poll for the result, or read the JSON body
   returned by the `/continue` response (status `202 Accepted`; poll for the
   final result).
5. After extraction (or timeout / cancellation) the Chromium process and
   temporary profile are destroyed automatically.

### API flow

```sh
# 1. Create a session
TOKEN=$(curl -s -X POST http://127.0.0.1:8080/v1/sessions \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","capture_screenshot":false}' \
  | jq -r .token)

echo "Session token: $TOKEN"

# 2. (Open noVNC in host browser and interact with the page)
#    http://127.0.0.1:6080/vnc.html

# 3. Signal continuation (triggers extraction)
curl -s -X POST "http://127.0.0.1:8080/v1/sessions/${TOKEN}/continue"

# 4. Poll for the result
curl -s "http://127.0.0.1:8080/v1/sessions/${TOKEN}"

# 5. Cancel a session instead
curl -s -X DELETE "http://127.0.0.1:8080/v1/sessions/${TOKEN}"
```

### Session states

| Status | Meaning |
|--------|---------|
| `waiting` | Session is live; Chromium is at the URL, waiting for `/continue` |
| `completed` | Extraction finished successfully |
| `failed` | Navigation, extraction, or a policy violation failed the session |
| `cancelled` | Explicit cancellation or service shutdown |

> **Policy enforcement:** All URL/DNS/request-interception protections remain
> active throughout the interactive phase.  A policy violation during manual
> browsing immediately fails and invalidates the session.

---

## noVNC option

The noVNC image lets you see and interact with the container's Chromium from
any host browser without installing an X server.  It is a **separate,
opt-in image** that adds Xvfb, x11vnc, and noVNC to the base image.

> ⚠️ **Security notice:** noVNC provides interactive access to the browser
> session and has **no built-in authentication**.  Always bind the noVNC port
> to `127.0.0.1`.  Use an authenticated tunnel (SSH `-L`, mTLS proxy, VPN) if
> remote access is required.

### Build the noVNC image

```sh
# Docker
docker build -f Dockerfile.novnc -t chrome-control:novnc .

# Podman
podman build -f Dockerfile.novnc -t chrome-control:novnc .
```

### Run the noVNC image

```sh
# Docker – bind both ports to localhost only
docker run --rm \
  --name chrome-control-novnc \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:6080:6080 \
  --read-only \
  --tmpfs /var/tmp/chrome-control:rw,exec,size=512m,mode=1777 \
  --tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m,mode=1777 \
  --tmpfs /dev/shm:rw,size=256m,mode=1777 \
  --tmpfs /tmp:rw,size=64m,mode=1777 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --pids-limit 512 \
  --memory 1g \
  ghcr.io/groovy-sky/chrome-control:main-novnc
```

```sh
# Podman equivalent
podman run --rm \
  --name chrome-control-novnc \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:6080:6080 \
  --read-only \
  --tmpfs /var/tmp/chrome-control:rw,exec,size=512m,mode=1777 \
  --tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m,mode=1777 \
  --tmpfs /dev/shm:rw,size=256m,mode=1777 \
  --tmpfs /tmp:rw,size=64m,mode=1777 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --pids-limit 512 \
  --memory 1g \
  ghcr.io/groovy-sky/chrome-control:main-novnc
```

### Environment variables (noVNC image)

All base-image variables apply, plus:

| Variable | Default | Description |
|----------|---------|-------------|
| `NOVNC_PORT` | `6080` | Port the noVNC web interface listens on |
| `DISPLAY` | `:99` | Virtual X display used by Chromium |
| `INTERACTIVE_TIMEOUT_SECONDS` | `300` | Session timeout in seconds |
| `MAX_INTERACTIVE_SESSIONS` | `2` | Max concurrent interactive sessions |

### Full CAPTCHA workflow example

```sh
# 1. Start the noVNC container
docker run --rm \
  --name cc-novnc \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:6080:6080 \
  --tmpfs /var/tmp/chrome-control:rw,exec,size=512m,mode=1777 \
  --tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m,mode=1777 \
  --tmpfs /dev/shm:rw,size=256m,mode=1777 \
  --tmpfs /tmp:rw,size=64m,mode=1777 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  ghcr.io/groovy-sky/chrome-control:main-novnc &

# 2. Wait for readiness
until curl -sf http://127.0.0.1:8080/readyz > /dev/null; do sleep 1; done

# 3. Create an interactive session
TOKEN=$(curl -s -X POST http://127.0.0.1:8080/v1/sessions \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/captcha-page"}' \
  | jq -r .token)

# 4. Open http://127.0.0.1:6080/vnc.html in your host browser
#    (the web-root URL http://127.0.0.1:6080/ may return a harmless 404)
#    Solve the CAPTCHA in the Chromium window shown by noVNC.
#    (noVNC displays the container's Chromium – it is NOT a tab in your
#    local Chrome; your mouse/keyboard events are forwarded to the container.)

# 5. After solving, trigger extraction
curl -s -X POST "http://127.0.0.1:8080/v1/sessions/${TOKEN}/continue"

# 6. Poll until completed
curl -s "http://127.0.0.1:8080/v1/sessions/${TOKEN}" | jq .
```

### Published image tags

| Event | Tag example |
|-------|------------|
| Push to `main` | `ghcr.io/groovy-sky/chrome-control:main-novnc` |
| Release `v1.2.3` | `ghcr.io/groovy-sky/chrome-control:1.2.3-novnc` |
| Pull request | built only, not published |

> The base image (`main`, `1.2.3`, `latest`) is unchanged by this feature.

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

## GUI / headful mode (local debugging)

The worker is **task-oriented**: it launches a fresh Chromium process per task
and terminates it after the task (and optional debug hold) completes. It is not
a persistent interactive browser.

When `HEADFUL=true`, Chromium opens a visible window instead of running
headless. This requires a running X server and a valid `DISPLAY` variable. The
container uses the existing Chromium binary — no display server is bundled.

`DEBUG_HOLD_SECONDS` keeps the window open for the specified number of seconds
after extraction (and screenshot, if requested) finishes. The hold is
cancellable: it respects task timeout and client disconnection. It does **not**
apply to readiness probes.

### Podman on Windows (X server example)

**Prerequisites:**

1. Install an X server on Windows, e.g. **VcXsrv** or **X410**.
2. Start XLaunch → Multiple windows → Start no client → check *Disable access
   control* (**private networks only — never on public/shared networks**).
3. Allow the X server through Windows Firewall on private networks only.

> ⚠️ **Security:** Disabling X access control allows any local process to
> capture your screen and inject input. Never expose X11 TCP port 6000 on
> public or untrusted networks.

```powershell
podman run --rm `
  --name chrome-control-gui `
  -p 127.0.0.1:8080:8080 `
  -e HEADFUL=true `
  -e DEBUG_HOLD_SECONDS=30 `
  -e DISPLAY=host.containers.internal:0.0 `
  --tmpfs /var/tmp/chrome-control:rw,exec,size=512m,mode=1777 `
  --tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m,mode=1777 `
  --tmpfs /dev/shm:rw,size=256m,mode=1777 `
  --tmpfs /tmp:rw,size=64m,mode=1777 `
  --cap-drop ALL `
  --security-opt no-new-privileges `
  --pids-limit 512 `
  --memory 1g `
  --cpus 1 `
  chrome-control
```

Submit a task:

```powershell
$body = @{
    task_id = "gui-test"
    url = "https://example.com"
    capture_screenshot = $false
    max_text_chars = 2000
} | ConvertTo-Json

Invoke-RestMethod `
    -Method Post `
    -Uri "http://localhost:8080/v1/tasks" `
    -ContentType "application/json" `
    -Body $body
```

The Chromium window will appear on the Windows desktop and close automatically
after `DEBUG_HOLD_SECONDS` seconds (or sooner if the task times out or the
client disconnects).

---

## Docker

The existing `Dockerfile` builds the HTTP worker (`cmd/worker`).

### Run from GitHub Container Registry

Pre-built HTTP worker images are published to GitHub Container Registry (GHCR)
at `ghcr.io/groovy-sky/chrome-control` by the Docker publishing workflow.

Pull the image built from the latest `main` branch commit:

```sh
docker pull ghcr.io/groovy-sky/chrome-control:main
```

Run it with the required writable temporary filesystems and recommended
container restrictions:

```sh
docker run --rm \
  --name chrome-control \
  -p 127.0.0.1:8080:8080 \
  --read-only \
  --tmpfs /var/tmp/chrome-control:rw,exec,size=512m,mode=1777 \
  --tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m,mode=1777 \
  --tmpfs /dev/shm:rw,size=256m,mode=1777 \
  --tmpfs /tmp:rw,size=64m,mode=1777 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --pids-limit 512 \
  --memory 1g \
  --cpus 1 \
  ghcr.io/groovy-sky/chrome-control:main
```

Verify the service and submit a task:

```sh
curl http://localhost:8080/readyz

curl -X POST http://localhost:8080/v1/tasks \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"ghcr-test","url":"https://example.com"}'
```

Version tags are also published when a Git tag matching `v*.*.*` is pushed. For
example, Git tag `v1.2.3` publishes image tags including `1.2.3`, `1.2`, `1`,
and `latest`. For reproducible deployments, prefer a full version or digest:

```sh
docker pull ghcr.io/groovy-sky/chrome-control:1.2.3
```

The same image can be run with Podman by replacing `docker` with `podman` in
the commands above.

If the package is private or otherwise requires authentication, create a GitHub
personal access token with `read:packages`, then log in before pulling:

```sh
export CR_PAT=YOUR_TOKEN
printf '%s' "$CR_PAT" | docker login ghcr.io -u YOUR_GITHUB_USERNAME --password-stdin
```

> **Host requirement:** The Chromium sandbox requires unprivileged user
> namespaces. On Linux, ensure they are enabled on the container host. Keep
> `CAP_SYS_ADMIN` and `CAP_NET_ADMIN` dropped, and enforce egress restrictions
> with an external firewall or proxy.

To add the MCP binary to your own image:

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
