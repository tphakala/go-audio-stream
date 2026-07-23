package rtsp

import (
	"errors"
	"strconv"
)

// state is the client lifecycle state, guarded by Client.mu.
type state int

const (
	// stateIdle is the state after a successful Dial: ready for Describe.
	stateIdle state = iota
	// stateDescribed is the state after Describe: ready for Setup.
	stateDescribed
	// stateSetup is the state after at least one Setup: ready for more Setup
	// or Play.
	stateSetup
	// statePlaying is the state after Play: frames flow.
	statePlaying
	// stateClosed is the terminal state after Close, a fatal error, a server
	// TEARDOWN, or watchdog expiry.
	stateClosed
)

// String returns the lowercase state name used in diagnostics and StateError.
func (s state) String() string {
	switch s {
	case stateIdle:
		return "idle"
	case stateDescribed:
		return "described"
	case stateSetup:
		return "setup"
	case statePlaying:
		return "playing"
	case stateClosed:
		return "closed"
	default:
		return "state(" + strconv.Itoa(int(s)) + ")"
	}
}

// RTSP method tokens the client sends. Hoisted to constants so the state
// machine and request builders share one spelling.
const (
	methodOptions  = "OPTIONS"
	methodDescribe = "DESCRIBE"
	methodSetup    = "SETUP"
	methodPlay     = "PLAY"
	methodTeardown = "TEARDOWN"
)

// ErrInvalidState is the sentinel a StateError matches under errors.Is.
var ErrInvalidState = errors.New("rtsp: method not valid in current state")

// StateError reports a lifecycle method called in a state that does not
// permit it (for example Play before Setup). It matches errors.Is against
// ErrInvalidState.
type StateError struct {
	// Method is the lifecycle method that was rejected.
	Method string
	// State is the state name at the time of the call.
	State string
}

// Error satisfies error.
func (e *StateError) Error() string {
	return "rtsp: " + e.Method + " not valid in state " + e.State
}

// Is reports whether target is ErrInvalidState, so callers can match any
// StateError with errors.Is(err, ErrInvalidState).
func (e *StateError) Is(target error) bool {
	return target == ErrInvalidState
}

// legalIn reports whether the named lifecycle method may run in state s.
// Describe runs only from idle; Setup from described or setup; Play only from
// setup. The closed state is terminal, enforced by the leading guard rather
// than left to emerge from the individual arms. OPTIONS and TEARDOWN are
// absent deliberately: they are not lifecycle transitions and never go through
// advance.
func legalIn(method string, s state) bool {
	if s == stateClosed {
		return false
	}
	switch method {
	case methodDescribe:
		return s == stateIdle
	case methodSetup:
		return s == stateDescribed || s == stateSetup
	case methodPlay:
		return s == stateSetup
	default:
		return false
	}
}

// destState is the state a legal method transitions into, and whether the
// method is one advance knows. Returning the second value rather than
// defaulting to stateClosed matters: legalIn and destState enumerate the same
// method set by hand, so a method added to one and forgotten in the other
// would otherwise report success while silently terminating the session.
func destState(method string) (state, bool) {
	switch method {
	case methodDescribe:
		return stateDescribed, true
	case methodSetup:
		return stateSetup, true
	case methodPlay:
		return statePlaying, true
	default:
		return stateIdle, false
	}
}
