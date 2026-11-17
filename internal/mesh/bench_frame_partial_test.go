// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// timeoutErr is a net.Error that reports a timeout, the way quic-go's
// deadline error does when a stream read runs past its deadline.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "deadline exceeded" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// scriptedReader hands out a fixed list of (bytes, error) steps. It is used
// to reproduce a read deadline that fires in the MIDDLE of a frame.
type scriptedReader struct {
	steps []scriptedStep
	i     int
	// pendingErr is an error that the previous step produced after its
	// bytes were delivered. bufio returns it with the next read.
	pendingErr error
}

type scriptedStep struct {
	data []byte
	err  error
}

// scriptedReader hands out a fixed list of steps. Each step is either a
// chunk of bytes or an error.
//
// The reader models what a QUIC stream actually does when a read deadline
// fires: the bytes that arrived before the deadline are returned FIRST, and
// the timeout error surfaces on the NEXT read. That is bufio's rule too —
// bufio.Reader keeps a pending error and returns it with the following
// read, so a reader that returned (data, err) together would not
// reproduce the real sequence.
func (r *scriptedReader) Read(p []byte) (int, error) {
	// A pending error from the previous call comes first.
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return 0, err
	}
	if r.i >= len(r.steps) {
		return 0, io.EOF
	}
	s := r.steps[r.i]
	r.i++
	if len(s.data) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		return 0, io.EOF
	}
	n := copy(p, s.data)
	// The rest of the chunk stays for the next call.
	if n < len(s.data) {
		s.data = s.data[n:]
		r.steps[r.i-1] = s
		return n, nil
	}
	// The whole chunk was delivered; its error surfaces on the next read.
	if s.err != nil {
		r.pendingErr = s.err
	}
	return n, nil
}

// TestBenchFrameSurvivesAReadDeadlineInsideAFrame pins that a read deadline
// firing in the middle of a frame does not corrupt the frame stream.
//
// The bench receive loop arms a one-second read deadline and, on a timeout,
// simply tries again. Over a real WAN that deadline does fire — sometimes
// in the middle of a frame header, sometimes in the middle of a frame body.
//
// io.ReadFull consumes the bytes it did read before the error arrived, and
// those bytes are gone: the retry then reads a fresh header from the
// middle of the old one, gets a garbage length, and the bench fails with
// "mesh: invalid bench frame size" — or "EOF" if the garbage length
// happens to be huge.
//
// This is only visible over a real link. A loopback test never sees a
// deadline fire mid-frame, which is why the local bench test passed while
// the real one failed at some sizes and not others (3 MiB worked, 4 MiB
// failed, 6 MiB worked).
func TestBenchFrameSurvivesAReadDeadlineInsideAFrame(t *testing.T) {
	// One data frame: a 4-byte length prefix, a tag byte, and a body.
	body := []byte{benchTagData}
	body = append(body, make([]byte, 40)...)
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)

	// The deadline fires after two bytes of the header have arrived. The
	// remaining header bytes and the body follow.
	src := &scriptedReader{steps: []scriptedStep{
		{data: frame[0:2], err: timeoutErr{}},
		{data: frame[2:], err: nil},
	}}
	br := bufio.NewReader(src)

	reader := &benchFrameReader{}
	var got []byte
	var lastErr error
	// The production loop's shape: a timeout means "no data yet, try
	// again", anything else is fatal.
	for attempt := 0; attempt < 5; attempt++ {
		got, lastErr = reader.next(br)
		if lastErr == nil {
			break
		}
		if ne, ok := lastErr.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
			break
		}
	}

	if lastErr != nil {
		t.Fatalf("the frame did not survive a mid-frame deadline: %v", lastErr)
	}
	if len(got) != len(body) {
		t.Fatalf("frame body length = %d, want %d", len(got), len(body))
	}
	if got[0] != benchTagData {
		t.Fatalf("frame tag = %d, want %d", got[0], benchTagData)
	}
}

// TestBenchFrameSurvivesADeadlineInsideTheBody is the same property for the
// body, which is the larger of the two reads.
func TestBenchFrameSurvivesADeadlineInsideTheBody(t *testing.T) {
	body := []byte{benchTagData}
	body = append(body, make([]byte, 1000)...)
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)

	src := &scriptedReader{steps: []scriptedStep{
		{data: frame[:4], err: nil},
		{data: frame[4 : 4+300], err: timeoutErr{}},
		{data: frame[4+300:], err: nil},
	}}
	br := bufio.NewReader(src)

	reader := &benchFrameReader{}
	var got []byte
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		got, lastErr = reader.next(br)
		if lastErr == nil {
			break
		}
		if ne, ok := lastErr.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
			break
		}
	}

	if lastErr != nil {
		t.Fatalf("the frame body did not survive a mid-body deadline: %v", lastErr)
	}
	if len(got) != len(body) {
		t.Fatalf("frame body length = %d, want %d", len(got), len(body))
	}
}

// TestBenchFrameRejectsAnOversizedLength keeps the guard the reader
// actually needs: a length that cannot be real is still an error.
func TestBenchFrameRejectsAnOversizedLength(t *testing.T) {
	src := &scriptedReader{steps: []scriptedStep{
		{data: []byte{0xff, 0xff, 0xff, 0xff}, err: nil},
	}}
	br := bufio.NewReader(src)
	reader := &benchFrameReader{}
	_, err := reader.next(br)
	if err == nil {
		t.Fatal("an oversized frame length was accepted")
	}
	if !errors.Is(err, err) { // keep the import honest
		_ = err
	}
}
