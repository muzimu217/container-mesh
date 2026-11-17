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

// oldBenchFrameReader is the implementation this package used before the
// mid-frame deadline fix: a plain io.ReadFull, with no carry-over.
//
// It is kept here so the new tests can be shown to fail against the OLD
// behaviour — a red test that has never been red is not evidence. The
// production copy was replaced; this one exists only to be measured
// against.
type oldBenchFrameReader struct {
	buf []byte
}

func (r *oldBenchFrameReader) next(br *bufio.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(br, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > benchFrameMax+1 {
		return nil, errors.New("mesh: invalid bench frame size")
	}
	if cap(r.buf) < int(n) {
		r.buf = make([]byte, n)
	}
	body := r.buf[:n]
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

// TestOldBenchFrameReaderLosesTheFrameAcrossADeadline records the OLD
// behaviour: a read deadline that fires in the middle of a frame destroys
// it, because io.ReadFull consumed the bytes it did read.
//
// This test asserts the OLD behaviour on purpose. It is the reference the
// new tests are measured against — if this ever starts failing, the old
// implementation changed and the comparison lost its meaning.
func TestOldBenchFrameReaderLosesTheFrameAcrossADeadline(t *testing.T) {
	body := []byte{benchTagData}
	body = append(body, make([]byte, 40)...)
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)

	// Two bytes of the header arrive, then the deadline fires, then the
	// rest of the frame.
	src := &scriptedReader{steps: []scriptedStep{
		{data: frame[0:2]},
		{data: nil, err: timeoutErr{}},
		{data: frame[2:]},
	}}
	br := bufio.NewReader(src)

	reader := &oldBenchFrameReader{}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		_, lastErr = reader.next(br)
		if lastErr == nil {
			break
		}
		if ne, ok := lastErr.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
			break
		}
	}

	if lastErr == nil {
		t.Fatal("the old reader unexpectedly survived a mid-frame deadline")
	}
	t.Logf("the old reader failed with: %v", lastErr)
}
