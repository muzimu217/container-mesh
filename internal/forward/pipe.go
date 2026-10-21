// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// halfCloser is implemented by connections that can shut down one direction
// without dropping the other. TCP connections and yamux streams both do.
type halfCloser interface {
	CloseWrite() error
}

// pipe copies bytes in both directions until both sides are done.
//
// The three properties that make this safe rather than merely short:
//
//  1. Half-close is propagated, not emulated. When one direction reaches EOF
//     the other side gets CloseWrite, so an HTTP/1.0 client that sends its
//     request and then shuts down its write side still receives the response.
//     Closing the whole connection there would truncate every such exchange.
//
//  2. A half-closed pipe has a deadline. After EOF on one side, the other side
//     gets drain to finish; if it does not, the connection is reset. Without
//     this a peer that never closes leaves a goroutine and 2x the stream window
//     held forever — which is exactly the leak a hostile client would farm.
//
//  3. Both directions are always drained before returning, so a caller that
//     waits on pipe has a guarantee that no goroutine outlives it.
func pipe(a, b io.ReadWriteCloser, drain time.Duration) error {
	var wg sync.WaitGroup
	wg.Add(2)

	// errs carries the first error per direction; a clean EOF is not an error.
	errs := make(chan error, 2)

	// copyDir copies src -> dst, then half-closes dst.
	copyDir := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		// Propagate the half-close regardless: the peer must learn that no
		// more data is coming, or it waits for a FIN that never arrives.
		if hc, ok := dst.(halfCloser); ok {
			_ = hc.CloseWrite()
		} else {
			_ = dst.Close()
		}
		if err != nil && !isClosedErr(err) {
			errs <- err
		}
	}

	go copyDir(a, b)
	go copyDir(b, a)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	if drain <= 0 {
		<-done
		return firstErr(errs)
	}

	// Give both directions drain to finish. A peer that half-closes and then
	// stops reading is the case this exists for: the write side would block
	// forever on a full window.
	select {
	case <-done:
	case <-time.After(drain):
		// Reset both ends so the blocked io.Copy calls return promptly.
		reset(a)
		reset(b)
		<-done
		return errors.Join(ErrPipeDrainTimeout, firstErr(errs))
	}
	return firstErr(errs)
}

// ErrPipeDrainTimeout reports that a half-closed pipe did not finish within the
// drain window and was reset. It is not a failure of the forward itself: the
// peer simply did not close.
var ErrPipeDrainTimeout = errors.New("forward: pipe drain timeout, connection reset")

// reset forces both directions to unblock by closing the connection.
func reset(c io.ReadWriteCloser) {
	if c == nil {
		return
	}
	// For a TCP connection, SetDeadline in the past is the cheapest way to
	// unblock a reader; Close is the fallback for everything else.
	if d, ok := c.(interface{ SetDeadline(time.Time) error }); ok {
		_ = d.SetDeadline(time.Now())
	}
	_ = c.Close()
}

// firstErr drains the error channel without blocking.
func firstErr(errs chan error) error {
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

// isClosedErr reports whether err is the ordinary "the other side went away"
// noise that every proxy sees and no operator wants to read.
func isClosedErr(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	// net.Error with a timeout from our own drain deadline.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
