// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// The benchmarks of the PRODUCTION SWITCH.
//
// The switch has THREE legs (the anchor, the requester endpoint and the
// target endpoint), and the ablation on the real host showed that all
// three have to move: the anchor's per-record cost was 1140 µs, the
// target's 1345 µs and the requester's 1778 µs — co-equal. Switching
// the anchor alone gives 1.85×; the measured full win (65.9–67.8 MiB/s
// against the product's 33.3–36.5) needs all three.
//
// So the benchmarks below measure all three legs, and each one is paired
// with the JSON path it replaced:
//
//   - BenchmarkRelayAnchorForward: the ANCHOR's forwarding path — the
//     splice (fast) against the JSON decode + re-encode (old).
//   - BenchmarkRelayEndpointWrite: the REQUESTER endpoint's write —
//     the binary frame against the JSON envelope.
//   - BenchmarkRelayEndpointRead: the TARGET endpoint's read — the
//     binary frame against the JSON envelope.
//
// The JSON numbers here are the numbers the production code used to
// produce, and the binary ones are what it produces now. The comparison
// is the point: a switch that cannot be measured is a switch nobody
// believes.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// benchDiscardSink is a far side that throws the bytes away.
//
// It is what the far side of a relay looks like from the anchor's point
// of view in a benchmark: the bytes are handed over, and measuring the
// hand-over is the point.
type benchDiscardSink struct {
	n int64
}

func (d *benchDiscardSink) Read(p []byte) (int, error)         { return 0, io.EOF }
func (d *benchDiscardSink) Write(p []byte) (int, error)        { d.n += int64(len(p)); return len(p), nil }
func (d *benchDiscardSink) Close() error                       { return nil }
func (d *benchDiscardSink) Abort() error                       { return nil }
func (d *benchDiscardSink) SetReadDeadline(t time.Time) error  { return nil }
func (d *benchDiscardSink) SetWriteDeadline(t time.Time) error { return nil }

// benchRecordWire builds the framed bytes of one relay record in the
// given format.
//
// The bytes are the ones the wire actually carries, so the benchmark
// measures the real thing: the length prefix and the body, in the
// format the leg speaks.
func benchRecordWire(t testing.TB, rec relayRecord, format RelaySpliceFormat) []byte {
	t.Helper()
	var body []byte
	switch format {
	case RelaySpliceFormatBinary:
		frame, err := relayFrameFromRecord(rec)
		if err != nil {
			t.Fatalf("relayFrameFromRecord: %v", err)
		}
		body, err = EncodeBinaryRelayFrame(frame)
		if err != nil {
			t.Fatalf("EncodeBinaryRelayFrame: %v", err)
		}
	case RelaySpliceFormatJSON:
		var err error
		body, err = json.Marshal(rec)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
	default:
		t.Fatalf("unknown format %d", uint8(format))
	}
	var wire bytes.Buffer
	if err := writeFrame(&wire, body); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	return wire.Bytes()
}

// BenchmarkRelayAnchorForward measures the ANCHOR's forwarding path.
//
// "JSON" is what the trunk did before the switch: read the record (which
// decodes it — json.Unmarshal plus base64 decode) and write it back out
// (json.Marshal plus base64 encode). "Binary/splice" is what it does
// now: read the 24-byte fixed header, learn the payload's length, and
// copy the bytes.
//
// The reader is REUSED across the iterations, because the relay's
// forwarding goroutine holds one reader for the whole relay — so the
// per-iteration cost here is the forwarding path's own cost, not the
// harness's.
func BenchmarkRelayAnchorForward(b *testing.B) {
	for _, size := range []int{1 << 10, 32 << 10, 256 << 10} {
		payload := bytes.Repeat([]byte{0x5A}, size)
		rec := relayRecord{Type: 3, Data: payload}

		jsonWire := benchRecordWire(b, rec, RelaySpliceFormatJSON)
		binWire := benchRecordWire(b, rec, RelaySpliceFormatBinary)

		b.Run("JSON/"+itoa(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			src := bytes.NewReader(nil)
			br := bufio.NewReader(src)
			sink := &benchDiscardSink{}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src.Reset(jsonWire)
				br.Reset(src)
				got, err := readRelayRecordAs(br, RelaySpliceFormatJSON)
				if err != nil {
					b.Fatalf("readRelayRecordAs: %v", err)
				}
				if err := writeRelayRecordAs(sink, got, RelaySpliceFormatJSON); err != nil {
					b.Fatalf("writeRelayRecordAs: %v", err)
				}
			}
		})

		b.Run("Binary/splice/"+itoa(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			src := bytes.NewReader(nil)
			br := bufio.NewReader(src)
			sink := &benchDiscardSink{}
			sp, err := NewRelaySplicer(RelaySpliceFormatBinary, RelaySpliceFormatBinary)
			if err != nil {
				b.Fatalf("NewRelaySplicer: %v", err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src.Reset(binWire)
				br.Reset(src)
				if _, err := sp.ForwardOne(br, sink); err != nil {
					b.Fatalf("ForwardOne: %v", err)
				}
			}
		})
	}
}

// BenchmarkRelayEndpointWrite measures the REQUESTER endpoint's write.
//
// The requester seals the plaintext and writes it as a record. The
// requester's per-record cost was 1778 µs — the highest of the three
// processes the switch touches — so this is the endpoint that matters
// most on the request side.
//
// The benchmark measures the ENCODING, not the seal: the seal is
// end-to-end cryptography whose cost did not change, and what the switch
// changed is the envelope around it.
func BenchmarkRelayEndpointWrite(b *testing.B) {
	for _, size := range []int{1 << 10, 32 << 10, 256 << 10} {
		payload := bytes.Repeat([]byte{0x5A}, size)
		rec := relayRecord{Type: 3, Data: payload}

		b.Run("JSON/"+itoa(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			sink := &benchDiscardSink{}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := writeRelayRecordAs(sink, rec, RelaySpliceFormatJSON); err != nil {
					b.Fatalf("writeRelayRecordAs: %v", err)
				}
			}
		})

		b.Run("Binary/"+itoa(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			sink := &benchDiscardSink{}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := writeRelayRecordAs(sink, rec, RelaySpliceFormatBinary); err != nil {
					b.Fatalf("writeRelayRecordAs: %v", err)
				}
			}
		})
	}
}

// BenchmarkRelayEndpointRead measures the TARGET endpoint's read.
//
// The target's per-record cost was 1345 µs. This is the DECODE half of
// it: the envelope is parsed off the wire and the payload is handed to
// the end-to-end layer.
func BenchmarkRelayEndpointRead(b *testing.B) {
	for _, size := range []int{1 << 10, 32 << 10, 256 << 10} {
		payload := bytes.Repeat([]byte{0x5A}, size)
		rec := relayRecord{Type: 3, Data: payload}

		jsonWire := benchRecordWire(b, rec, RelaySpliceFormatJSON)
		binWire := benchRecordWire(b, rec, RelaySpliceFormatBinary)

		b.Run("JSON/"+itoa(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			src := bytes.NewReader(nil)
			br := bufio.NewReader(src)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src.Reset(jsonWire)
				br.Reset(src)
				if _, err := readRelayRecordAs(br, RelaySpliceFormatJSON); err != nil {
					b.Fatalf("readRelayRecordAs: %v", err)
				}
			}
		})

		b.Run("Binary/"+itoa(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			src := bytes.NewReader(nil)
			br := bufio.NewReader(src)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src.Reset(binWire)
				br.Reset(src)
				if _, err := readRelayRecordAs(br, RelaySpliceFormatBinary); err != nil {
					b.Fatalf("readRelayRecordAs: %v", err)
				}
			}
		})
	}
}

// BenchmarkRelayMixedVersionFallback measures the TRANSLATE path.
//
// It is the price of a half-upgraded deployment: the anchor decodes the
// record and re-encodes it in the far leg's format. It is measured so
// the cost is KNOWN rather than assumed — the design doc's claim was
// that it costs about the same as the old JSON path, and this is where
// that claim is checked.
func BenchmarkRelayMixedVersionFallback(b *testing.B) {
	const size = 32 << 10
	payload := bytes.Repeat([]byte{0x5A}, size)
	rec := relayRecord{Type: 3, Data: payload}

	// The requester's leg speaks the binary frame; the target's leg
	// speaks JSON. The anchor has to translate.
	binWire := benchRecordWire(b, rec, RelaySpliceFormatBinary)

	b.Run("Binary-to-JSON", func(b *testing.B) {
		b.SetBytes(int64(size))
		b.ReportAllocs()
		src := bytes.NewReader(nil)
		br := bufio.NewReader(src)
		sink := &benchDiscardSink{}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			src.Reset(binWire)
			br.Reset(src)
			got, err := readRelayRecordAs(br, RelaySpliceFormatBinary)
			if err != nil {
				b.Fatalf("readRelayRecordAs: %v", err)
			}
			if err := writeRelayRecordAs(sink, got, RelaySpliceFormatJSON); err != nil {
				b.Fatalf("writeRelayRecordAs: %v", err)
			}
		}
	})
}

// itoa is a tiny helper so the benchmark names can carry the payload
// size without pulling strconv into the package's test surface.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
