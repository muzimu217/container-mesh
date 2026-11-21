// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The benchmarks of the BYTE SPLICE.
//
// They mirror the shape of the relay frame's own benchmarks
// (BenchmarkRelayRecordForward and BenchmarkRelayRecordForwardStream in
// relayframe_test.go), so the numbers are comparable:
//
//   - "JSON" is what the trunk does TODAY: read the record, decode it
//     (json.Unmarshal + base64 decode), and re-encode it (json.Marshal +
//     base64 encode). This is the number the splice has to beat.
//   - "Binary/splice" is what the trunk will do with the splice: read
//     the 24-byte header, copy the rest.
//
// The two differ in one more thing than the frame's own benchmarks, and
// it is the thing that matters: the splice's number includes the BYTES
// GOING OUT to the far side. A benchmark that stopped at "I know the
// length" would measure almost nothing; the copy is the work.
// ---------------------------------------------------------------------------

// discardSink is a far side that throws the bytes away.
//
// It is what the far side of a relay looks like from the anchor's point
// of view in a benchmark: the bytes are handed over, and measuring the
// hand-over is the point.
type discardSink struct {
	n int64
}

func (d *discardSink) Read(p []byte) (int, error)       { return 0, io.EOF }
func (d *discardSink) Write(p []byte) (int, error)      { d.n += int64(len(p)); return len(p), nil }
func (d *discardSink) Close() error                     { return nil }
func (d *discardSink) Abort() error                     { return nil }
func (d *discardSink) SetReadDeadline(time.Time) error  { return nil }
func (d *discardSink) SetWriteDeadline(time.Time) error { return nil }

// BenchmarkRelaySpliceForward measures the forwarding path the way the
// trunk runs it: the record comes off a stream and the bytes go out to
// the far side.
//
// The "JSON" variant is the trunk's current path, reproduced exactly:
// read the record with readRelayRecord (which decodes it), then write it
// with writeRelayRecord (which re-encodes it). The "Binary/splice"
// variant is the splice.
//
// The two are measured on the SAME payload, so the numbers are directly
// comparable.
func BenchmarkRelaySpliceForward(b *testing.B) {
	const size = 32 << 10
	payload := bytes.Repeat([]byte{0x5A}, size)
	rec := relayRecord{Type: 3, Data: payload}
	frame, err := relayFrameFromRecord(rec)
	if err != nil {
		b.Fatalf("relayFrameFromRecord: %v", err)
	}
	jsonBody, err := json.Marshal(rec)
	if err != nil {
		b.Fatalf("json.Marshal: %v", err)
	}
	binBody, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		b.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	var jsonWire, binWire bytes.Buffer
	if err := writeFrame(&jsonWire, jsonBody); err != nil {
		b.Fatalf("writeFrame: %v", err)
	}
	if err := writeFrame(&binWire, binBody); err != nil {
		b.Fatalf("writeFrame: %v", err)
	}

	b.Run("JSON", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			br := bufio.NewReader(bytes.NewReader(jsonWire.Bytes()))
			got, err := readRelayRecord(br)
			if err != nil {
				b.Fatalf("readRelayRecord: %v", err)
			}
			sink := &discardSink{}
			if err := writeRelayRecord(sink, got); err != nil {
				b.Fatalf("writeRelayRecord: %v", err)
			}
		}
	})

	b.Run("Binary/splice", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		sp, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatBinary)
		if err != nil {
			b.Fatalf("NewRelaySplicer: %v", err)
		}
		for i := 0; i < b.N; i++ {
			br := bufio.NewReader(bytes.NewReader(binWire.Bytes()))
			sink := &discardSink{}
			if _, err := sp.ForwardOne(br, sink); err != nil {
				b.Fatalf("ForwardOne: %v", err)
			}
		}
	})
}

// BenchmarkRelaySpliceForwardSizes sweeps the payload sizes, the way the
// frame's own round-trip benchmark does.
//
// The sizes are the ones that matter: 1 KiB is a small message, 32 KiB
// is a typical bulk chunk, and 256 KiB is a large one.
func BenchmarkRelaySpliceForwardSizes(b *testing.B) {
	sizes := []struct {
		name    string
		payload int
	}{
		{"1KiB", 1 << 10},
		{"32KiB", 32 << 10},
		{"256KiB", 256 << 10},
	}
	for _, size := range sizes {
		payload := bytes.Repeat([]byte{0x5A}, size.payload)
		rec := relayRecord{Type: 3, Data: payload}
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			b.Fatalf("relayFrameFromRecord: %v", err)
		}
		jsonBody, err := json.Marshal(rec)
		if err != nil {
			b.Fatalf("json.Marshal: %v", err)
		}
		binBody, err := EncodeBinaryRelayFrame(frame)
		if err != nil {
			b.Fatalf("EncodeBinaryRelayFrame: %v", err)
		}
		var jsonWire, binWire bytes.Buffer
		if err := writeFrame(&jsonWire, jsonBody); err != nil {
			b.Fatalf("writeFrame: %v", err)
		}
		if err := writeFrame(&binWire, binBody); err != nil {
			b.Fatalf("writeFrame: %v", err)
		}

		b.Run("JSON/"+size.name, func(b *testing.B) {
			b.SetBytes(int64(size.payload))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				br := bufio.NewReader(bytes.NewReader(jsonWire.Bytes()))
				got, err := readRelayRecord(br)
				if err != nil {
					b.Fatalf("readRelayRecord: %v", err)
				}
				sink := &discardSink{}
				if err := writeRelayRecord(sink, got); err != nil {
					b.Fatalf("writeRelayRecord: %v", err)
				}
			}
		})

		b.Run("Binary/splice/"+size.name, func(b *testing.B) {
			b.SetBytes(int64(size.payload))
			b.ReportAllocs()
			sp, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatBinary)
			if err != nil {
				b.Fatalf("NewRelaySplicer: %v", err)
			}
			for i := 0; i < b.N; i++ {
				br := bufio.NewReader(bytes.NewReader(binWire.Bytes()))
				sink := &discardSink{}
				if _, err := sp.ForwardOne(br, sink); err != nil {
					b.Fatalf("ForwardOne: %v", err)
				}
			}
		})
	}
}

// BenchmarkRelaySpliceHeaderOnly measures the part of the splice that is
// NOT the copy: learning the record's length.
//
// This is the number the frame's own "Binary" variant reports (12.57
// ns/op): read 24 bytes, learn the length, do not decode. It is the
// floor — the copy on top of it is what the splice actually costs, and
// the two together are the honest number.
func BenchmarkRelaySpliceHeaderOnly(b *testing.B) {
	const size = 32 << 10
	payload := bytes.Repeat([]byte{0x5A}, size)
	frame, err := relayFrameFromRecord(relayRecord{Type: 3, Data: payload})
	if err != nil {
		b.Fatalf("relayFrameFromRecord: %v", err)
	}
	binBody, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		b.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hdr, err := PeekRelayFrame(binBody)
		if err != nil {
			b.Fatalf("PeekRelayFrame: %v", err)
		}
		_ = hdr
	}
}

// BenchmarkRelaySpliceSteadyState measures the splice the way the relay
// actually runs it: ONE buffered reader, REUSED for every record.
//
// The per-iteration bufio.NewReader in the benchmark above is the
// harness's own allocation, not the splice's — the relay's forwarding
// goroutine holds one reader for the whole relay (relayFlow.reqBr /
// tgtBr) and reads record after record out of it.
//
// This benchmark is the honest per-record number: it reuses the reader
// and resets it, so what is left is the splice's own cost.
func BenchmarkRelaySpliceSteadyState(b *testing.B) {
	const size = 32 << 10
	payload := bytes.Repeat([]byte{0x5A}, size)
	frame, err := relayFrameFromRecord(relayRecord{Type: 3, Data: payload})
	if err != nil {
		b.Fatalf("relayFrameFromRecord: %v", err)
	}
	binBody, err := EncodeBinaryRelayFrame(frame)
	if err != nil {
		b.Fatalf("EncodeBinaryRelayFrame: %v", err)
	}
	var wire bytes.Buffer
	if err := writeFrame(&wire, binBody); err != nil {
		b.Fatalf("writeFrame: %v", err)
	}
	raw := wire.Bytes()

	b.Run("Binary/splice", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		sp, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatBinary)
		if err != nil {
			b.Fatalf("NewRelaySplicer: %v", err)
		}
		src := bytes.NewReader(raw)
		br := bufio.NewReader(src)
		sink := &discardSink{}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			src.Reset(raw)
			br.Reset(src)
			if _, err := sp.ForwardOne(br, sink); err != nil {
				b.Fatalf("ForwardOne: %v", err)
			}
		}
	})
}
