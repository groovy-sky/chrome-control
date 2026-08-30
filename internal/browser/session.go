// Package browser – session.go manages interactive browsing sessions.
//
// An interactive session navigates to a URL and then pauses, keeping Chromium
// alive, so the operator can interact with the page (e.g. to solve a CAPTCHA).
// The caller obtains a cryptographically random opaque token, then signals
// continuation via that token.  On continuation, extraction runs and the result
// is stored on the session.  A configurable timeout and explicit cancel both
// terminate the session.
package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/groovy-sky/chrome-control/internal/models"
)

const (
	DefaultInteractiveTimeout  = 5 * time.Minute
	DefaultMaxInteractiveSessions = 2
)

// sessionState holds runtime state for one interactive session.
type sessionState struct {
	token      string
	req        models.SessionRequest
	status     string
	result     *models.BrowserResult
	continueCh chan struct{} // closed to signal continuation
	cancelCh   chan struct{} // closed to signal explicit cancellation
	doneCh     chan struct{} // closed by the runner goroutine when it finishes
	mu         sync.Mutex
}

// SessionManager keeps track of live interactive sessions.
type SessionManager struct {
	worker  *Worker
	timeout time.Duration
	slots   chan struct{} // bounded concurrency
	logger  *slog.Logger

	mu       sync.Mutex
	sessions map[string]*sessionState

	// TestHookRunSession, when non-nil, is called at the start of each
	// runSession goroutine before the browser is started. This is intended
	// only for unit tests that need to synchronise goroutine scheduling.
	TestHookRunSession func()
}

// NewSessionManager creates a SessionManager backed by the given Worker.
func NewSessionManager(w *Worker, timeout time.Duration, maxSessions int, logger *slog.Logger) *SessionManager {
	if timeout <= 0 {
		timeout = DefaultInteractiveTimeout
	}
	if maxSessions < 1 {
		maxSessions = DefaultMaxInteractiveSessions
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SessionManager{
		worker:   w,
		timeout:  timeout,
		slots:    make(chan struct{}, maxSessions),
		logger:   logger,
		sessions: make(map[string]*sessionState),
	}
}

// Create validates the request, acquires a concurrency slot, registers a new
// session and starts the background browser goroutine.  It returns the opaque
// token or an error.
func (m *SessionManager) Create(ctx context.Context, req models.SessionRequest) (string, *models.BrowserError) {
	if req.URL == "" {
		return "", models.NewError(models.CodeInvalidRequest, "url is required")
	}
	if req.MaxTextChars < 0 {
		return "", models.NewError(models.CodeInvalidRequest, "max_text_chars must not be negative")
	}

	select {
	case m.slots <- struct{}{}:
	default:
		return "", models.NewError(models.CodeOverloaded, "interactive session limit reached")
	}

	token, err := GenerateToken()
	if err != nil {
		<-m.slots
		return "", models.NewError(models.CodeBrowserStartFailed, "could not generate session token")
	}

	s := &sessionState{
		token:      token,
		req:        req,
		status:     models.StatusWaiting,
		continueCh: make(chan struct{}),
		cancelCh:   make(chan struct{}),
		doneCh:     make(chan struct{}),
	}

	m.mu.Lock()
	m.sessions[token] = s
	m.mu.Unlock()

	go m.runSession(ctx, s)

	return token, nil
}

// Continue signals the waiting session to proceed with extraction.  It returns
// an error if the token is unknown, or if the session is no longer in the
// waiting state.
func (m *SessionManager) Continue(token string) *models.BrowserError {
	s := m.get(token)
	if s == nil {
		return models.NewError(models.CodeSessionNotFound, "session not found")
	}
	s.mu.Lock()
	if s.status != models.StatusWaiting {
		st := s.status
		s.mu.Unlock()
		if st == models.StatusCompleted || st == models.StatusFailed {
			return models.NewError(models.CodeSessionExpired, "session has already completed")
		}
		return models.NewError(models.CodeSessionExpired, "session is no longer waiting")
	}
	s.mu.Unlock()

	// Non-blocking: if already closed (double-continue race), ignore.
	select {
	case <-s.continueCh:
	default:
		close(s.continueCh)
	}
	return nil
}

// Cancel terminates a waiting or in-progress session.
func (m *SessionManager) Cancel(token string) *models.BrowserError {
	s := m.get(token)
	if s == nil {
		return models.NewError(models.CodeSessionNotFound, "session not found")
	}
	s.mu.Lock()
	if s.status != models.StatusWaiting {
		s.mu.Unlock()
		return models.NewError(models.CodeSessionExpired, "session is no longer waiting")
	}
	s.mu.Unlock()

	select {
	case <-s.cancelCh:
	default:
		close(s.cancelCh)
	}
	return nil
}

// Status returns a snapshot of the session state and result.
func (m *SessionManager) Status(token string) (*models.SessionStatus, *models.BrowserError) {
	s := m.get(token)
	if s == nil {
		return nil, models.NewError(models.CodeSessionNotFound, "session not found")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &models.SessionStatus{
		Token:  token,
		Status: s.status,
	}
	if s.status == models.StatusCompleted || s.status == models.StatusFailed {
		out.Result = s.result
	}
	return out, nil
}

// Shutdown cancels all waiting sessions; blocks until each runner goroutine
// has exited.
func (m *SessionManager) Shutdown() {
	m.mu.Lock()
	tokens := make([]string, 0, len(m.sessions))
	for t := range m.sessions {
		tokens = append(tokens, t)
	}
	m.mu.Unlock()

	for _, t := range tokens {
		s := m.get(t)
		if s == nil {
			continue
		}
		select {
		case <-s.cancelCh:
		default:
			close(s.cancelCh)
		}
		<-s.doneCh
	}
}

// get looks up a session by token without removing it.
func (m *SessionManager) get(token string) *sessionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[token]
}

// remove removes a session from the registry and releases its slot.
func (m *SessionManager) remove(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
	<-m.slots
}

// runSession is the goroutine that drives the browser for one interactive
// session.  It navigates to the URL, waits for a continue/cancel/timeout
// signal, and then either extracts the page or fails closed.
func (m *SessionManager) runSession(serviceCtx context.Context, s *sessionState) {
	if m.TestHookRunSession != nil {
		m.TestHookRunSession()
	}
	defer func() {
		close(s.doneCh)
		m.remove(s.token)
		// Redact token from logs; log only the first 8 chars.
		m.logger.Info("interactive session finished",
			slog.String("token_prefix", s.token[:8]+"…"),
			slog.String("status", s.status))
	}()

	setStatus := func(st string) {
		s.mu.Lock()
		s.status = st
		s.mu.Unlock()
	}
	fail := func(berr *models.BrowserError) {
		s.mu.Lock()
		s.status = models.StatusFailed
		s.result = &models.BrowserResult{Status: models.StatusFailed, Error: berr}
		s.mu.Unlock()
	}

	// Validate + navigate using a background browser run.
	result, browserCtx, cancelBrowser, cleanup := m.worker.startInteractive(serviceCtx, s.req)
	defer func() {
		cancelBrowser()
		if cleanup != nil {
			cleanup()
		}
	}()

	if result != nil {
		// Navigation failed.
		fail(result.Error)
		return
	}

	// Navigation succeeded; wait for continuation, cancellation, or timeout.
	setStatus(models.StatusWaiting)
	timer := time.NewTimer(m.timeout)
	defer timer.Stop()

	select {
	case <-s.continueCh:
		// Operator clicked "continue".
	case <-s.cancelCh:
		setStatus(models.StatusCancelled)
		return
	case <-timer.C:
		fail(models.NewError(models.CodeSessionExpired, "interactive session timed out"))
		return
	case <-serviceCtx.Done():
		setStatus(models.StatusCancelled)
		return
	}

	// Extract now that the operator is done.
	res := m.worker.extractAfterInteraction(browserCtx, s.req)
	s.mu.Lock()
	if res.Error != nil {
		s.status = models.StatusFailed
	} else {
		s.status = models.StatusCompleted
	}
	s.result = res
	s.mu.Unlock()
}

// GenerateToken returns a 32-byte cryptographically random hex token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
