// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrTransportSwitched is returned by a reader whose transport was replaced
// underneath it. It is not a failure: the read pump uses it to restart on the
// new transport.
var ErrTransportSwitched = errors.New("peer: transport switched")

// switchable is a MessageConn whose underlying transport can be replaced once,
// after a direct UDP path has been established.
//
// Why a wrapper rather than a plain field swap: the read pump is blocked
// inside Recv when the switch happens, and there is no way to interrupt that
// call except by closing the socket it is reading. Closing the old transport
// makes Recv return an error; the pump then asks this wrapper whether a new
// transport is installed and, if so, continues instead of failing the session.
type switchable struct {
	mu      sync.RWMutex
	current MessageConn
	// switched is set once a replacement has been installed. It is read on
	// the error path of Recv, so it must be atomic-friendly.
	switched atomic.Bool
	// sendMu serialises Send across a swap: a send that started on the old
	// transport must not be interleaved with one on the new one.
	sendMu sync.Mutex
}

func newSwitchable(c MessageConn) *switchable {
	return &switchable{current: c}
}

// Send writes to the current transport.
func (s *switchable) Send(b []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.RLock()
	c := s.current
	s.mu.RUnlock()
	return c.Send(b)
}

// Recv reads from the current transport. On error it reports
// ErrTransportSwitched when a replacement is already in place, so the caller
// can retry rather than tear the session down.
func (s *switchable) Recv() ([]byte, error) {
	s.mu.RLock()
	c := s.current
	s.mu.RUnlock()

	msg, err := c.Recv()
	if err != nil && s.switched.Load() {
		// Distinguish "the old socket was closed by us" from a real failure.
		s.mu.RLock()
		replaced := s.current != c
		s.mu.RUnlock()
		if replaced {
			return nil, ErrTransportSwitched
		}
	}
	return msg, err
}

// Close closes the current transport.
func (s *switchable) Close() error {
	s.mu.RLock()
	c := s.current
	s.mu.RUnlock()
	return c.Close()
}

// Switch replaces the transport. It closes the old one after installing the
// new one, which unblocks any Recv parked on the old socket.
func (s *switchable) Switch(next MessageConn) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	s.mu.Lock()
	old := s.current
	s.current = next
	s.mu.Unlock()
	s.switched.Store(true)

	if old != nil {
		// Closing after the swap: a reader that was blocked will wake up, see
		// switched=true and a different current transport, and retry.
		_ = old.Close()
	}
	return nil
}

// Switched reports whether a replacement has been installed.
func (s *switchable) Switched() bool { return s.switched.Load() }
