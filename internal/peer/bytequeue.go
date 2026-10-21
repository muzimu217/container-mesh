// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer

import (
	"io"
	"sync"
)

// byteQueue is a bounded FIFO of bytes with blocking writes.
//
// It backs both directions of the record pump. Blocking when full is the
// correct behaviour, not a bug: a slow transport must slow yamux's sendLoop,
// which slows each stream's Write, which slows the application's write(). A
// queue that dropped or grew instead would either corrupt the byte stream or
// let one slow peer pin unbounded memory.
//
// Reads return whatever is available rather than waiting for a full buffer, so
// a caller that wants "one record" gets one record-sized piece of the stream
// without an extra framing layer.
type byteQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte // ring storage, len == capacity
	head   int    // index of the oldest byte
	n      int    // bytes currently held
	closed bool
	err    error // nil until closed; io.EOF for a clean close
}

func newByteQueue(capacity int) *byteQueue {
	if capacity <= 0 {
		capacity = 1
	}
	q := &byteQueue{buf: make([]byte, capacity)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Write appends all of b, blocking until there is room. It never returns a
// short count without an error.
func (q *byteQueue) Write(b []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	total := 0
	for len(b) > 0 {
		for q.n == len(q.buf) && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			if total > 0 {
				// Report what made it in; the caller's next call sees the
				// error. Matches io.Writer's contract.
				return total, nil
			}
			return 0, q.closeErr()
		}
		// Contiguous free space at the tail, wrapping at most once.
		tail := (q.head + q.n) % len(q.buf)
		free := len(q.buf) - q.n
		contig := len(q.buf) - tail
		if contig > free {
			contig = free
		}
		if contig > len(b) {
			contig = len(b)
		}
		copy(q.buf[tail:tail+contig], b[:contig])
		q.n += contig
		b = b[contig:]
		total += contig
		q.cond.Broadcast()
	}
	return total, nil
}

// Put appends one whole record. It is Write with a clearer name at the call
// site that knows it is enqueuing a record.
func (q *byteQueue) Put(p []byte) error {
	_, err := q.Write(p)
	return err
}

// Read copies up to len(b) bytes, blocking until at least one byte is
// available. It returns io.EOF (or the close error) once the queue is drained
// and closed.
func (q *byteQueue) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.n == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.n == 0 {
		return 0, q.closeErr()
	}
	n := q.n
	if n > len(b) {
		n = len(b)
	}
	// At most two copies: from head to the end, then from the start.
	first := len(q.buf) - q.head
	if first > n {
		first = n
	}
	copy(b[:first], q.buf[q.head:q.head+first])
	if rest := n - first; rest > 0 {
		copy(b[first:n], q.buf[:rest])
	}
	q.head = (q.head + n) % len(q.buf)
	q.n -= n
	q.cond.Broadcast()
	return n, nil
}

// Get is Read, named for the record pump's use.
func (q *byteQueue) Get(b []byte) (int, error) { return q.Read(b) }

// TryGet is Read without blocking: ok is false when nothing is queued.
//
// This is what makes batching non-blocking. The writer blocks for the first
// record and then drains only what is already there, so a lone keystroke never
// waits for records that may never arrive.
func (q *byteQueue) TryGet(b []byte) (n int, ok bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.n == 0 {
		if q.closed {
			return 0, false, q.closeErr()
		}
		return 0, false, nil
	}
	if len(b) == 0 {
		return 0, true, nil
	}
	n = q.n
	if n > len(b) {
		n = len(b)
	}
	first := len(q.buf) - q.head
	if first > n {
		first = n
	}
	copy(b[:first], q.buf[q.head:q.head+first])
	if rest := n - first; rest > 0 {
		copy(b[first:n], q.buf[:rest])
	}
	q.head = (q.head + n) % len(q.buf)
	q.n -= n
	q.cond.Broadcast()
	return n, true, nil
}

// Len reports how many bytes are queued. For tests and metrics.
func (q *byteQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.n
}

// Close marks the queue closed with io.EOF. It is idempotent, and it never
// overwrites an error recorded by CloseWithError.
func (q *byteQueue) Close() { q.CloseWithError(io.EOF) }

// CloseWithError marks the queue closed and wakes every waiter. Idempotent:
// the first reason wins, so a real transport error is not masked by the clean
// close that follows it.
func (q *byteQueue) CloseWithError(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	if err == nil {
		err = io.EOF
	}
	q.err = err
	q.cond.Broadcast()
}

// closeErr must be called with q.mu held.
func (q *byteQueue) closeErr() error {
	if q.err != nil {
		return q.err
	}
	return io.EOF
}
