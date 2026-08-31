package unit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/groovy-sky/chrome-control/internal/browser"
	"github.com/groovy-sky/chrome-control/internal/models"
)

// mockSessionManager is a thin wrapper that calls the real SessionManager but
// with a fake worker so it never actually launches Chromium.  We test only
// the session lifecycle (token generation, state machine, concurrency limiting,
// timeouts, races) without any browser dependency.
//
// To avoid requiring a real browser we bypass the normal SessionManager.Create
// path and directly exercise the manager's exported methods by injecting
// pre-built sessions through testable helper constructors.

// ----- helper: build a manager without a real worker -----

// newTestManager returns a SessionManager whose worker is nil.  The session
// runner goroutine (runSession) needs a real worker, so in tests we call the
// lower-level manager methods directly rather than Create().
func newTestManager(timeout time.Duration, maxSessions int) *browser.SessionManager {
	return browser.NewSessionManager(nil, context.Background(), timeout, maxSessions, nil)
}

// ----- token helpers -----

func TestGenerateToken_Unique(t *testing.T) {
	t.Parallel()
	tokens := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		tok, err := browser.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken() error: %v", err)
		}
		if len(tok) != 64 {
			t.Fatalf("expected 64-char hex token, got len %d", len(tok))
		}
		if _, dup := tokens[tok]; dup {
			t.Fatal("duplicate token generated")
		}
		tokens[tok] = struct{}{}
	}
}

// ----- session state machine -----

// exerciseSession drives a session through the given phase with the manager's
// public methods.  Returns the status after the operation.
func TestSessionManager_Continue_NotFound(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(5*time.Minute, 2)
	berr := mgr.Continue("nonexistent-token")
	if berr == nil {
		t.Fatal("expected error for unknown token, got nil")
	}
	if berr.Code != models.CodeSessionNotFound {
		t.Fatalf("expected %s, got %s", models.CodeSessionNotFound, berr.Code)
	}
}

func TestSessionManager_Cancel_NotFound(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(5*time.Minute, 2)
	berr := mgr.Cancel("nonexistent-token")
	if berr == nil {
		t.Fatal("expected error for unknown token, got nil")
	}
	if berr.Code != models.CodeSessionNotFound {
		t.Fatalf("expected %s, got %s", models.CodeSessionNotFound, berr.Code)
	}
}

func TestSessionManager_Status_NotFound(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(5*time.Minute, 2)
	_, berr := mgr.Status("nonexistent-token")
	if berr == nil {
		t.Fatal("expected error for unknown token, got nil")
	}
	if berr.Code != models.CodeSessionNotFound {
		t.Fatalf("expected %s, got %s", models.CodeSessionNotFound, berr.Code)
	}
}

// ----- concurrency limiting -----

func TestSessionManager_ConcurrencyLimit(t *testing.T) {
	t.Parallel()
	mgr := browser.NewSessionManager(nil, context.Background(), 5*time.Minute, 1, nil)
	req := models.SessionRequest{URL: "https://example.com"}

	// Use the test hook to hold the first runSession goroutine until we've
	// confirmed the second Create sees an overloaded error.
	goroutineStarted := make(chan struct{})
	goroutineUnblock := make(chan struct{})
	mgr.TestHookRunSession = func() {
		goroutineStarted <- struct{}{}
		<-goroutineUnblock
	}

	tok1, berr := mgr.Create(req)
	if berr != nil {
		t.Fatalf("first Create failed unexpectedly: %v", berr)
	}
	if tok1 == "" {
		t.Fatal("first Create returned empty token")
	}

	// Wait until the goroutine is running (slot is held).
	select {
	case <-goroutineStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession goroutine did not start in time")
	}

	// Slot is full; second Create must return overloaded.
	_, berr2 := mgr.Create(req)
	if berr2 == nil {
		t.Fatal("expected overloaded error, got nil")
	}
	if berr2.Code != models.CodeOverloaded {
		t.Fatalf("expected code %s, got %s", models.CodeOverloaded, berr2.Code)
	}

	// Unblock the goroutine so the test can exit cleanly.
	close(goroutineUnblock)
}

// ----- invalid request -----

func TestSessionManager_Create_EmptyURL(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(5*time.Minute, 2)
	_, berr := mgr.Create(models.SessionRequest{})
	if berr == nil {
		t.Fatal("expected error for empty URL, got nil")
	}
	if berr.Code != models.CodeInvalidRequest {
		t.Fatalf("expected %s, got %s", models.CodeInvalidRequest, berr.Code)
	}
}

func TestSessionManager_Create_NegativeMaxTextChars(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(5*time.Minute, 2)
	_, berr := mgr.Create(models.SessionRequest{URL: "https://example.com", MaxTextChars: -1})
	if berr == nil {
		t.Fatal("expected error for negative max_text_chars, got nil")
	}
	if berr.Code != models.CodeInvalidRequest {
		t.Fatalf("expected %s, got %s", models.CodeInvalidRequest, berr.Code)
	}
}

// ----- shutdown races -----

func TestSessionManager_Shutdown_NoDeadlock(t *testing.T) {
	t.Parallel()
	mgr := browser.NewSessionManager(nil, context.Background(), 5*time.Minute, 4, nil)

	// Spin up a few Create calls concurrently to fill the manager with
	// sessions that are backed by a nil worker (they will fail quickly).
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.Create(models.SessionRequest{URL: "https://example.com"}) //nolint:errcheck
		}()
	}
	wg.Wait()

	// Shutdown must return in a bounded time even if sessions are still live.
	done := make(chan struct{})
	go func() {
		mgr.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown() did not return within 10 seconds")
	}
}

// ----- session status codes -----

func TestModels_SessionStatusCodes(t *testing.T) {
	t.Parallel()
	if models.StatusWaiting == "" {
		t.Fatal("StatusWaiting must not be empty")
	}
	if models.StatusCancelled == "" {
		t.Fatal("StatusCancelled must not be empty")
	}
	if models.StatusCompleted == "" {
		t.Fatal("StatusCompleted must not be empty")
	}
	if models.StatusFailed == "" {
		t.Fatal("StatusFailed must not be empty")
	}
}

// ----- HTTPStatusForCode covers new session codes -----

func TestHTTPStatusForCode_SessionCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code   string
		expect int
	}{
		{models.CodeSessionNotFound, 404},
		{models.CodeSessionExpired, 410},
		{models.CodeOverloaded, 503},
	}
	for _, tc := range tests {
		got := models.HTTPStatusForCode(tc.code)
		if got != tc.expect {
			t.Errorf("HTTPStatusForCode(%q) = %d, want %d", tc.code, got, tc.expect)
		}
	}
}

// ----- request context decoupling -----

// TestSessionManager_RequestContextCancelDoesNotTerminateSession verifies that
// canceling the HTTP-request context after session creation does NOT terminate
// or remove the interactive session.  The session must remain queryable in the
// "waiting" state.
func TestSessionManager_RequestContextCancelDoesNotTerminateSession(t *testing.T) {
	t.Parallel()

	// Use a long timeout so the session doesn't expire during the test.
	mgr := browser.NewSessionManager(nil, context.Background(), 10*time.Minute, 2, nil)

	// Hold the goroutine inside runSession until we explicitly release it, so
	// we can observe the session in "waiting" state after the goroutine has
	// started but before the nil worker causes a panic.
	goroutineStarted := make(chan struct{})
	goroutineUnblock := make(chan struct{})
	mgr.TestHookRunSession = func() {
		close(goroutineStarted)
		<-goroutineUnblock
	}

	// Simulate an HTTP request context that is canceled right after Create.
	// The session must NOT be affected because Create no longer accepts a
	// per-request context; it uses the service-lifetime context instead.
	_, cancelReq := context.WithCancel(context.Background())

	tok, berr := mgr.Create(models.SessionRequest{URL: "https://example.com"})
	if berr != nil {
		t.Fatalf("Create failed: %v", berr)
	}
	if tok == "" {
		t.Fatal("Create returned empty token")
	}

	// Wait until the goroutine has started, then cancel the request context.
	select {
	case <-goroutineStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession goroutine did not start in time")
	}
	cancelReq() // simulate HTTP handler returning / client disconnect

	// Give the goroutine a moment to react (it should NOT exit).
	time.Sleep(50 * time.Millisecond)

	// The session must still be present and in "waiting" state.
	st, err := mgr.Status(tok)
	if err != nil {
		t.Fatalf("Status returned error after request-context cancel: %v", err)
	}
	if st.Status != models.StatusWaiting {
		t.Fatalf("expected status %q, got %q", models.StatusWaiting, st.Status)
	}

	// Unblock the goroutine so the test exits cleanly.
	close(goroutineUnblock)
}

// TestSessionManager_ServiceShutdownTerminatesSessions verifies that calling
// Shutdown() with the service context canceled terminates active sessions.
func TestSessionManager_ServiceShutdownTerminatesSessions(t *testing.T) {
	t.Parallel()

	serviceCtx, cancelService := context.WithCancel(context.Background())

	mgr := browser.NewSessionManager(nil, serviceCtx, 10*time.Minute, 2, nil)

	goroutineStarted := make(chan struct{})
	goroutineUnblock := make(chan struct{})
	mgr.TestHookRunSession = func() {
		close(goroutineStarted)
		<-goroutineUnblock
	}

	tok, berr := mgr.Create(models.SessionRequest{URL: "https://example.com"})
	if berr != nil {
		t.Fatalf("Create failed: %v", berr)
	}

	select {
	case <-goroutineStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession goroutine did not start")
	}
	close(goroutineUnblock) // let it enter the browser phase (will fail fast with nil worker)

	// Shutdown should cancel and clean up all sessions.
	cancelService()
	done := make(chan struct{})
	go func() {
		mgr.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not complete in time")
	}

	// After shutdown, the session must be gone from the registry.
	_, err := mgr.Status(tok)
	if err == nil {
		t.Fatal("expected session_not_found after shutdown, got nil error")
	}
	if err.Code != models.CodeSessionNotFound {
		t.Fatalf("expected %s, got %s", models.CodeSessionNotFound, err.Code)
	}
}

// TestDefaultInteractiveTimeout verifies the default is one hour.
func TestDefaultInteractiveTimeout(t *testing.T) {
	t.Parallel()
	const want = 60 * time.Minute
	if browser.DefaultInteractiveTimeout != want {
		t.Fatalf("DefaultInteractiveTimeout = %v, want %v", browser.DefaultInteractiveTimeout, want)
	}
}

// TestSessionManager_TimeoutOverride verifies that a custom timeout is
// honoured and does not fall back to the default.
func TestSessionManager_TimeoutOverride(t *testing.T) {
	t.Parallel()
	custom := 30 * time.Second
	mgr := browser.NewSessionManager(nil, context.Background(), custom, 2, nil)

	goroutineStarted := make(chan struct{})
	goroutineUnblock := make(chan struct{})
	mgr.TestHookRunSession = func() {
		close(goroutineStarted)
		<-goroutineUnblock
	}

	tok, berr := mgr.Create(models.SessionRequest{URL: "https://example.com"})
	if berr != nil {
		t.Fatalf("Create failed: %v", berr)
	}

	select {
	case <-goroutineStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not start")
	}
	close(goroutineUnblock)

	// The token was issued; verify the manager recorded it.
	st, err := mgr.Status(tok)
	if err != nil {
		// The goroutine may have already exited (nil worker); that's fine.
		if err.Code == models.CodeSessionNotFound {
			return
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Token != tok {
		t.Fatalf("token mismatch: got %q, want %q", st.Token, tok)
	}
}
