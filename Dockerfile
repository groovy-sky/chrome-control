# syntax=docker/dockerfile:1

# ---- build stage -------------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# ---- runtime stage -----------------------------------------------------------
FROM debian:bookworm-slim

# Chromium plus the fonts and certificates required to render public pages.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        chromium \
        ca-certificates \
        fonts-liberation \
        fonts-dejavu-core \
        tini \
    && rm -rf /var/lib/apt/lists/*

# The worker and Chromium run as a dedicated non-root user. The Chromium
# sandbox stays enabled; --no-sandbox is never passed.
RUN groupadd --system --gid 10001 worker \
    && useradd --system --uid 10001 --gid worker --home-dir /home/worker --create-home worker

COPY --from=build /out/worker /usr/local/bin/worker

# Writable mounts. The root filesystem is expected to be mounted read-only, so
# these two directories must be provided as tmpfs or writable volumes.
RUN mkdir -p /var/tmp/chrome-control /var/lib/chrome-control/artifacts \
    && chown -R worker:worker /var/tmp/chrome-control /var/lib/chrome-control

ENV CHROME_PATH=/usr/bin/chromium \
    ARTIFACT_DIR=/var/lib/chrome-control/artifacts \
    TMPDIR=/var/tmp/chrome-control \
    ADDR=:8080 \
    MAX_CONCURRENT_TASKS=4

USER worker
WORKDIR /home/worker
EXPOSE 8080

# tini reaps the Chromium process tree if any descendant outlives its parent.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/worker"]

# Deployment requirements (enforced by the container runtime, not the image):
#
#   docker run \
#     --read-only \
#     --tmpfs /var/tmp/chrome-control:rw,exec,size=512m \
#     --tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m \
#     --tmpfs /dev/shm:rw,size=256m \
#     --cap-drop ALL \
#     --security-opt no-new-privileges \
#     --pids-limit 512 --memory 1g --cpus 1 \
#     chrome-control
#
# * The Chromium sandbox requires unprivileged user namespaces on the host
#   (kernel.unprivileged_userns_clone=1). CAP_SYS_ADMIN and CAP_NET_ADMIN must
#   stay dropped.
# * Egress must be restricted by an external firewall or proxy so that Chromium
#   cannot reach loopback, private, link-local, CGNAT or cloud metadata
#   networks even if application-level validation is bypassed. Application-level
#   validation is defense in depth, not the security boundary.
# * Authentication for POST /v1/tasks is enforced at the reverse proxy or API
#   gateway in front of this container.
