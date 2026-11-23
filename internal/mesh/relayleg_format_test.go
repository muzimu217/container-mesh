package mesh

import (
	"bufio"
	"bytes"
	"testing"
)

// TestALegReportsItsOwnFormat is the regression test for the format
// mismatch that showed up as an intermittent failure.
//
// The symptom was memorable:
//
//	mesh: relay trunk: the join stream broke: mesh: frame of 1937009004
//	bytes exceeds the 1048576 byte cap
//
// 1937009004 is 0x7374696C — the four ASCII bytes "stil" (the first
// four letters of "still") read as a big-endian length. So a reader
// was reading PLAIN TEXT where a length prefix belongs: the reader and
// the writer were on DIFFERENT formats.
//
// The byte order matters for anyone hunting this in a log: the frame
// length is BIG endian (internal/mesh/direct.go:158 and :806), so the
// bytes on the wire were 73 74 69 6C = "stil". The little-endian
// reading of the same number is "lits" — a plausible-looking but
// wrong string to grep for.
//
// The mechanism: a leg's format is asked of it with
// relayFormatOfSession, which asks for the ALPN the leg's connection
// negotiated. A CONNECTION can say (quicSession.ALPN). A relayed SESSION
// could not — relaySession had no ALPN method — so the question came
// back empty and the caller FELL BACK TO THE BINARY FRAME.
//
// So a leg that had negotiated the OLD protocol was written as JSON (the
// writer knew the format) and read as binary (the reader did not).
// Whenever the old protocol was in play, the reader desynchronised.
func TestALegReportsItsOwnFormat(t *testing.T) {
	// A leg that speaks JSON must SAY so.
	legacy := &relaySession{format: RelaySpliceFormatJSON}
	if got := legacy.ALPN(); got != RelayALPN {
		t.Fatalf("a JSON leg reports the ALPN %q, want %q: a leg that cannot "+
			"say is read with the WRONG format, and the reader desynchronises",
			got, RelayALPN)
	}

	// And the format question must find it.
	got, err := relayFormatOfSession(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got != RelaySpliceFormatJSON {
		t.Fatalf("a JSON leg is read as %v, want JSON", got)
	}

	// A leg that speaks the binary frame says the new ALPN.
	modern := &relaySession{format: RelaySpliceFormatBinary}
	if got := modern.ALPN(); got != RelayALPN2 {
		t.Fatalf("a binary leg reports the ALPN %q, want %q", got, RelayALPN2)
	}
	got, err = relayFormatOfSession(modern)
	if err != nil {
		t.Fatal(err)
	}
	if got != RelaySpliceFormatBinary {
		t.Fatalf("a binary leg is read as %v, want the binary frame", got)
	}
}

// TestAJSONLegIsReadAsJSON is the end-to-end shape: a record written by
// a JSON leg must be READ by that same leg — the reader must not
// desynchronise on the very first record.
//
// The failure mode is exactly "the reader finds text where a length
// prefix belongs", so the assertion is that the read succeeds at all.
func TestAJSONLegIsReadAsJSON(t *testing.T) {
	// What a JSON leg writes.
	var wire bytes.Buffer
	rec := relayRecord{Type: 3, Data: []byte("the payload a JSON record carries")}
	if err := writeRelayRecordAs(&wire, rec, RelaySpliceFormatJSON); err != nil {
		t.Fatal(err)
	}

	// A reader for that leg.
	br := bufio.NewReader(&wire)
	reader := newFramedRecordReader(br, RelaySpliceFormatJSON)
	got, err := reader.nextRecord()
	if err != nil {
		t.Fatalf("a JSON leg's own record was not read back: %v "+
			"(a reader on the wrong format finds text where a length prefix belongs)",
			err)
	}
	if string(got.Data) != string(rec.Data) {
		t.Fatalf("the record read back carries %q, want %q", got.Data, rec.Data)
	}
}
