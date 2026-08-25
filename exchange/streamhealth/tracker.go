// Package streamhealth provides the small, thread-safe lifecycle shared by
// private order WebSocket implementations. It deliberately knows nothing
// about exchange protocols so adapters remain responsible for proving that a
// connection has authenticated and subscribed before marking it ready.
package streamhealth

import "sync"

// State describes the observable lifecycle of a private order stream.
type State string

const (
	StateStopped  State = "STOPPED"
	StateStarting State = "STARTING"
	StateReady    State = "READY"
	StateDegraded State = "DEGRADED"
	StateStopping State = "STOPPING"
)

// Tracker stores a private order stream's current lifecycle state.
// Its zero value is a stopped tracker.
type Tracker struct {
	mu        sync.RWMutex
	state     State
	lastError string
}

// Set updates the lifecycle state and optional diagnostic error.
func (t *Tracker) Set(state State, err error) {
	t.mu.Lock()
	t.state = state
	if err != nil {
		t.lastError = err.Error()
	} else if state == StateStarting || state == StateReady || state == StateStopped {
		t.lastError = ""
	}
	t.mu.Unlock()
}

// Ready reports whether authentication and subscription are currently alive.
func (t *Tracker) Ready() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state == StateReady
}

// State returns the current state. An unused tracker is stopped.
func (t *Tracker) State() State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.state == "" {
		return StateStopped
	}
	return t.state
}

// LastError returns the most recent lifecycle diagnostic.
func (t *Tracker) LastError() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastError
}
