// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"errors"
	"sync"
)

// memPipe is one endpoint of an in-memory message channel pair, used by tests
// to exercise the session logic without any network.
type memPipe struct {
	out     chan []byte
	in      chan []byte
	shared  *sharedClose
	recvErr error
}

// sharedClose lets both endpoints close the same underlying channel exactly
// once.
type sharedClose struct {
	once   sync.Once
	closed chan struct{}
}

func (s *sharedClose) Close() {
	s.once.Do(func() { close(s.closed) })
}

func newMemPipePair() (a, b *memPipe) {
	ab := make(chan []byte, 64)
	ba := make(chan []byte, 64)
	sc := &sharedClose{closed: make(chan struct{})}
	a = &memPipe{out: ab, in: ba, shared: sc}
	b = &memPipe{out: ba, in: ab, shared: sc}
	return a, b
}

func (p *memPipe) Send(msg []byte) error {
	cp := make([]byte, len(msg))
	copy(cp, msg)
	select {
	case <-p.shared.closed:
		return errors.New("mempipe: closed")
	default:
	}
	select {
	case p.out <- cp:
		return nil
	case <-p.shared.closed:
		return errors.New("mempipe: closed")
	}
}

func (p *memPipe) Recv() ([]byte, error) {
	select {
	case msg := <-p.in:
		return msg, nil
	case <-p.shared.closed:
		return nil, errors.New("mempipe: closed")
	}
}

func (p *memPipe) Close() error {
	p.shared.Close()
	return nil
}
