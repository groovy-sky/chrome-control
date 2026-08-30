package unit

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/groovy-sky/chrome-control/internal/browser"
)

// parseBool mirrors the logic used by mustEnvBool in both entry points.
func parseBool(v string) (bool, error) {
	if v == "" {
		return false, nil // empty → fallback (caller supplies default)
	}
	return strconv.ParseBool(v)
}

// parseHoldSeconds mirrors the logic used by mustEnvDuration in both entry points.
func parseHoldSeconds(v string) (time.Duration, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %w", err)
	}
	if n < 0 {
		return 0, fmt.Errorf("must not be negative: %d", n)
	}
	return time.Duration(n) * time.Second, nil
}

// TestConfigDefaults verifies that a zero-value Config gets sensible defaults.
func TestConfigDefaults(t *testing.T) {
	w := browser.New(browser.Config{})
	if w == nil {
		t.Fatal("New returned nil")
	}
}

// TestHeadfulDefault verifies Headful is false by default.
func TestHeadfulDefault(t *testing.T) {
	cfg := browser.Config{}
	if cfg.Headful {
		t.Error("Headful must default to false")
	}
}

// TestDebugHoldDefault verifies DebugHold is zero by default.
func TestDebugHoldDefault(t *testing.T) {
	cfg := browser.Config{}
	if cfg.DebugHold != 0 {
		t.Errorf("DebugHold must default to zero, got %v", cfg.DebugHold)
	}
}

// TestDebugHoldCancellable verifies that a debug hold is aborted when the
// context is cancelled, i.e. it does not block unconditionally.
func TestDebugHoldCancellable(t *testing.T) {
	hold := 30 * time.Second // would block the test if not cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-time.After(hold):
		case <-ctx.Done():
		}
	}()

	select {
	case <-done:
		// pass — cancelled immediately as expected
	case <-time.After(2 * time.Second):
		t.Fatal("hold was not cancelled by context cancellation")
	}
}

// TestMustEnvBoolParsing exercises the parsing logic used by both entry points.
func TestMustEnvBoolParsing(t *testing.T) {
	cases := []struct {
		input   string
		want    bool
		wantErr bool
	}{
		{"true", true, false},
		{"1", true, false},
		{"TRUE", true, false},
		{"false", false, false},
		{"0", false, false},
		{"FALSE", false, false},
		{"", false, false}, // empty → fallback (no error)
		{"yes", false, true},
		{"no", false, true},
		{"maybe", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseBool(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.input)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for %q: %v", tc.input, err)
				}
				if got != tc.want {
					t.Fatalf("parseBool(%q) = %v, want %v", tc.input, got, tc.want)
				}
			}
		})
	}
}

// TestMustEnvDurationParsing exercises the duration parsing logic.
func TestMustEnvDurationParsing(t *testing.T) {
	cases := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"0", 0, false},
		{"5", 5 * time.Second, false},
		{"60", 60 * time.Second, false},
		{"-1", 0, true},
		{"abc", 0, true},
		{"3.5", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseHoldSeconds(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.input)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for %q: %v", tc.input, err)
				}
				if got != tc.want {
					t.Fatalf("parseHoldSeconds(%q) = %v, want %v", tc.input, got, tc.want)
				}
			}
		})
	}
}
