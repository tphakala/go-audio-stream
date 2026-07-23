package rtsp

import (
	"errors"
	"testing"
)

// Expected wire spellings for the state names. Declared here deliberately
// SEPARATE from state.go's String, not shared with it: a constant shared
// between implementation and test makes the assertion tautological, which is
// what this file previously did (String returned the same constant the test
// compared against, so renaming a state kept the suite green).
const (
	wantIdle      = "idle"
	wantDescribed = "described"
)

func TestStateString(t *testing.T) {
	t.Parallel()
	// Asserted against literals, not against the constants String returns.
	// Sharing one constant between implementation and test made this
	// tautological: renaming a state kept the test green while every
	// StateError message changed.
	cases := map[state]string{
		stateIdle:      wantIdle,
		stateDescribed: wantDescribed,
		stateSetup:     "setup",
		statePlaying:   "playing",
		stateClosed:    "closed",
		state(99):      "state(99)",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("state(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}

func TestDestState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		want   state
		wantOK bool
	}{
		{methodDescribe, stateDescribed, true},
		{methodSetup, stateSetup, true},
		{methodPlay, statePlaying, true},
		{methodOptions, stateIdle, false},
		{methodTeardown, stateIdle, false},
		{"BOGUS", stateIdle, false},
	}
	for _, tt := range cases {
		got, ok := destState(tt.method)
		if ok != tt.wantOK {
			t.Errorf("destState(%q) ok = %v, want %v", tt.method, ok, tt.wantOK)
		}
		if ok && got != tt.want {
			t.Errorf("destState(%q) = %v, want %v", tt.method, got, tt.want)
		}
	}
}

// The full legal lifecycle, asserted through advance so destState's mapping is
// pinned rather than inferred. Swapping the SETUP and PLAY arms of destState
// used to leave the whole suite green.
func TestAdvanceLifecycle(t *testing.T) {
	t.Parallel()
	c := &Client{state: stateIdle}
	steps := []struct {
		method string
		want   state
	}{
		{methodDescribe, stateDescribed},
		{methodSetup, stateSetup},
		{methodSetup, stateSetup},
		{methodPlay, statePlaying},
	}
	for _, st := range steps {
		if err := c.advance(st.method); err != nil {
			t.Fatalf("advance(%q): %v", st.method, err)
		}
		if c.state != st.want {
			t.Fatalf("after %q state = %v, want %v", st.method, c.state, st.want)
		}
	}
}

// Every method is refused once the session is closed, including the ones that
// are legal earlier in the lifecycle.
func TestAdvanceClosedIsTerminal(t *testing.T) {
	t.Parallel()
	for _, m := range []string{methodDescribe, methodSetup, methodPlay, methodOptions, methodTeardown} {
		c := &Client{state: stateClosed}
		if err := c.advance(m); !errors.Is(err, ErrInvalidState) {
			t.Errorf("advance(%q) from closed = %v, want ErrInvalidState", m, err)
		}
		if c.state != stateClosed {
			t.Errorf("advance(%q) mutated a closed state to %v", m, c.state)
		}
	}
}

func TestLegalIn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		s      state
		want   bool
	}{
		{methodDescribe, stateIdle, true},
		{methodDescribe, stateDescribed, false},
		{methodDescribe, stateSetup, false},
		{methodDescribe, statePlaying, false},
		{methodDescribe, stateClosed, false},

		{methodSetup, stateIdle, false},
		{methodSetup, stateDescribed, true},
		{methodSetup, stateSetup, true},
		{methodSetup, statePlaying, false},
		{methodSetup, stateClosed, false},

		{methodPlay, stateIdle, false},
		{methodPlay, stateDescribed, false},
		{methodPlay, stateSetup, true},
		{methodPlay, statePlaying, false},
		{methodPlay, stateClosed, false},
	}
	for _, c := range cases {
		if got := legalIn(c.method, c.s); got != c.want {
			t.Errorf("legalIn(%q, %s) = %v, want %v", c.method, c.s, got, c.want)
		}
	}
}

func TestAdvanceGuards(t *testing.T) {
	t.Parallel()

	// A legal transition moves the state and returns nil.
	c := &Client{state: stateIdle}
	if err := c.advance(methodDescribe); err != nil {
		t.Fatalf("advance DESCRIBE from idle: %v", err)
	}
	if c.state != stateDescribed {
		t.Fatalf("after DESCRIBE state = %s, want described", c.state)
	}

	// An illegal transition returns a *StateError and does not change state.
	err := c.advance(methodPlay)
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("advance PLAY from described: err = %v, want *StateError", err)
	}
	if se.Method != methodPlay || se.State != wantDescribed {
		t.Errorf("StateError = {Method:%q State:%q}, want {PLAY described}", se.Method, se.State)
	}
	if !errors.Is(err, ErrInvalidState) {
		t.Errorf("StateError does not match ErrInvalidState")
	}
	if c.state != stateDescribed {
		t.Errorf("illegal advance changed state to %s", c.state)
	}
}

func TestStateErrorIs(t *testing.T) {
	t.Parallel()
	e := &StateError{Method: "PLAY", State: wantIdle}
	if !errors.Is(e, ErrInvalidState) {
		t.Error("StateError.Is(ErrInvalidState) = false, want true")
	}
	if errors.Is(e, errors.New("other")) {
		t.Error("StateError.Is(other) = true, want false")
	}
	if e.Error() == "" {
		t.Error("StateError.Error() is empty")
	}
}
