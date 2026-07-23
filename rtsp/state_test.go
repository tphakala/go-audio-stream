package rtsp

import (
	"errors"
	"testing"
)

func TestStateString(t *testing.T) {
	t.Parallel()
	cases := map[state]string{
		stateIdle:      stateNameIdle,
		stateDescribed: stateNameDescribed,
		stateSetup:     stateNameSetup,
		statePlaying:   stateNamePlaying,
		stateClosed:    stateNameClosed,
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("state(%d).String() = %q, want %q", int(s), got, want)
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
	if se.Method != methodPlay || se.State != stateNameDescribed {
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
	e := &StateError{Method: "PLAY", State: stateNameIdle}
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
