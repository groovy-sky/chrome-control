// Command worker exposes the isolated Chromium browser worker over HTTP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/groovy-sky/chrome-control/internal/artifacts"
	"github.com/groovy-sky/chrome-control/internal/browser"
	"github.com/groovy-sky/chrome-control/internal/envutil"
	"github.com/groovy-sky/chrome-control/internal/models"
)

const (
	maxRequestBodyBytes  = 64 * 1024
	defaultAddr          = ":8080"
	defaultMaxConcurrent = 4
	shutdownDrainTimeout = 45 * time.Second
)

type server struct {
	worker   *browser.Worker
	sessions *browser.SessionManager
	store    *artifacts.Store
	logger   *slog.Logger
	slots    chan struct{}
	ready    atomic.Bool
	inFlight atomic.Int64
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	artifactDir := envString("ARTIFACT_DIR", filepath.Join(os.TempDir(), "chrome-control-artifacts"))
	store, err := artifacts.New(artifactDir)
	if err != nil {
		logger.Error("could not create artifact store", slog.String("error", err.Error()))
		os.Exit(1)
	}

	w := browser.New(browser.Config{
		ChromePath: os.Getenv("CHROME_PATH"),
		Headful:    envutil.Bool(logger, "HEADFUL", false),
		DebugHold:  envutil.HoldSeconds(logger, "DEBUG_HOLD_SECONDS", 0),
		Artifacts:  store,
		Logger:     logger,
	})

	maxConcurrent := envInt("MAX_CONCURRENT_TASKS", defaultMaxConcurrent)
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}

	interactiveTimeout := envutil.HoldSeconds(logger, "INTERACTIVE_TIMEOUT_SECONDS", browser.DefaultInteractiveTimeout)
	maxInteractive := envInt("MAX_INTERACTIVE_SESSIONS", browser.DefaultMaxInteractiveSessions)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sessionMgr := browser.NewSessionManager(w, ctx, interactiveTimeout, maxInteractive, logger)

	srv := &server{
		worker:   w,
		sessions: sessionMgr,
		store:    store,
		logger:   logger,
		slots:    make(chan struct{}, maxConcurrent),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", srv.handleTask)
	mux.HandleFunc("POST /v1/sessions", srv.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions/{token}", srv.handleGetSession)
	mux.HandleFunc("POST /v1/sessions/{token}/continue", srv.handleContinueSession)
	mux.HandleFunc("DELETE /v1/sessions/{token}", srv.handleCancelSession)
	mux.HandleFunc("GET /healthz", srv.handleHealthz)
	mux.HandleFunc("GET /readyz", srv.handleReadyz)

	httpServer := &http.Server{
		Addr:              envString("ADDR", defaultAddr),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Readiness is granted once Chromium has been shown to start.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.Probe(ctx); err != nil {
			logger.Error("readiness probe failed: chromium could not be started",
				slog.String("error", err.Error()))
			return
		}
		srv.ready.Store(true)
		logger.Info("worker is ready", slog.Int("max_concurrent_tasks", maxConcurrent))
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting worker", slog.String("addr", httpServer.Addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("http server failed", slog.String("error", err.Error()))
		os.Exit(1)
	case <-ctx.Done():
	}

	// Stop advertising readiness, then drain in-flight tasks.
	srv.ready.Store(false)
	logger.Info("shutting down", slog.Int64("in_flight", srv.inFlight.Load()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
	}
	sessionMgr.Shutdown()
	logger.Info("shutdown complete")
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not_ready"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

func (s *server) handleTask(w http.ResponseWriter, r *http.Request) {
	if !validJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, "", models.NewError(models.CodeInvalidRequest, "Content-Type must be application/json"),
			http.StatusUnsupportedMediaType)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req models.BrowserRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, "", models.NewError(models.CodeInvalidRequest, "request body is not valid JSON for this API"),
			http.StatusBadRequest)
		return
	}
	if dec.More() {
		writeError(w, req.TaskID, models.NewError(models.CodeInvalidRequest, "request body must contain a single JSON object"),
			http.StatusBadRequest)
		return
	}
	if berr := browser.ValidateRequest(&req); berr != nil {
		writeError(w, req.TaskID, berr, models.HTTPStatusForCode(berr.Code))
		return
	}

	// Bounded concurrency: overload fails fast, there is no queue.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeError(w, req.TaskID, models.NewError(models.CodeOverloaded, "concurrent task limit reached"),
			http.StatusServiceUnavailable)
		return
	}

	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)

	// The task is cancelled as soon as the client disconnects.
	result := s.worker.Run(r.Context(), req)

	// Artifacts are retained only until the response has been written.
	if result.ScreenshotArtifactID != "" {
		defer func(id string) {
			if err := s.store.Delete(id); err != nil {
				s.logger.Error("could not delete artifact", slog.String("error", err.Error()))
			}
		}(result.ScreenshotArtifactID)
	}

	status := http.StatusOK
	if result.Error != nil {
		status = models.HTTPStatusForCode(result.Error.Code)
	}
	writeJSON(w, status, result)
}

func validJSONContentType(value string) bool {
	if value == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

func writeError(w http.ResponseWriter, taskID string, berr *models.BrowserError, status int) {
	writeJSON(w, status, &models.BrowserResult{
		TaskID: taskID,
		Status: models.StatusFailed,
		Error:  berr,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().Error("could not write response", slog.String("error", err.Error()))
	}
}

func envString(key, fallback string) string {
	return envutil.String(key, fallback)
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// handleCreateSession handles POST /v1/sessions.
func (s *server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if !validJSONContentType(r.Header.Get("Content-Type")) {
		writeSessionError(w, models.NewError(models.CodeInvalidRequest, "Content-Type must be application/json"),
			http.StatusUnsupportedMediaType)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req models.SessionRequest
	if err := dec.Decode(&req); err != nil {
		writeSessionError(w, models.NewError(models.CodeInvalidRequest, "request body is not valid JSON for this API"),
			http.StatusBadRequest)
		return
	}

	token, berr := s.sessions.Create(req)
	if berr != nil {
		writeSessionError(w, berr, models.HTTPStatusForCode(berr.Code))
		return
	}

	writeJSON(w, http.StatusAccepted, &models.SessionResponse{
		Token:  token,
		Status: models.StatusWaiting,
	})
}

// handleGetSession handles GET /v1/sessions/{token}.
func (s *server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	status, berr := s.sessions.Status(token)
	if berr != nil {
		writeSessionError(w, berr, models.HTTPStatusForCode(berr.Code))
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleContinueSession handles POST /v1/sessions/{token}/continue.
func (s *server) handleContinueSession(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	berr := s.sessions.Continue(token)
	if berr != nil {
		writeSessionError(w, berr, models.HTTPStatusForCode(berr.Code))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "continuing"})
}

// handleCancelSession handles DELETE /v1/sessions/{token}.
func (s *server) handleCancelSession(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	berr := s.sessions.Cancel(token)
	if berr != nil {
		writeSessionError(w, berr, models.HTTPStatusForCode(berr.Code))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeSessionError(w http.ResponseWriter, berr *models.BrowserError, status int) {
	writeJSON(w, status, &models.SessionStatus{
		Status: models.StatusFailed,
		Result: &models.BrowserResult{Status: models.StatusFailed, Error: berr},
	})
}
