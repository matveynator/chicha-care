package upgrade

// Package upgrade defines the small file-based handshake shared by old and new binaries.
// We prefer this lightweight channel over sockets so the flow works everywhere.

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

// State constants written to the coordination file.
const (
	StatePending = "pending"
	StateStandby = "standby"
	StateGo      = "go"
	StateRunning = "running"
	StateAbort   = "abort"
)

// ErrAborted indicates that the upgrade should be cancelled.
var ErrAborted = errors.New("upgrade aborted")

// WriteState overwrites the file with the provided marker.
func WriteState(path, state string) error {
	return os.WriteFile(path, []byte(state), 0o600)
}

// ReadState loads the current marker from disk.
func ReadState(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// WaitForGo is called by the new process: announce standby then wait for go.
func WaitForGo(ctx context.Context, path string) error {
	if err := WriteState(path, StateStandby); err != nil {
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			state, err := ReadState(path)
			if err != nil {
				continue
			}
			switch state {
			case StateGo:
				return nil
			case StateAbort:
				return ErrAborted
			}
		}
	}
}

// WaitForState waits until the file matches one of the expected states.
func WaitForState(ctx context.Context, path string, states ...string) (string, error) {
	wanted := map[string]struct{}{}
	for _, state := range states {
		wanted[state] = struct{}{}
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			state, err := ReadState(path)
			if err != nil {
				continue
			}
			if _, ok := wanted[state]; ok {
				return state, nil
			}
		}
	}
}
