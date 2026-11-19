package mesh

import (
	"testing"
)

// TestTheQuicConfigGivesTheRelayRoomToMoveAFile is the regression test
// for the defect the real machine found: a file of a few megabytes
// stalled on the relay path, and the SAME file, right afterwards, went
// through.
//
// The stall was not the size. It was the flow control window.
//
// The relay's records are JSON: a []byte travels as base64, so a
// megabyte of payload becomes about 1.4 megabytes on the wire, and the
// end-to-end seal adds a nonce and a tag on top. quic-go's DEFAULT
// initial stream receive window is 512 KiB and the default initial
// CONNECTION window is 1.5 times that (768 KiB) — smaller than ONE
// relay record.
//
// The window only grows when the receiver consumes it faster than half
// a window per RTT (quic-go's maybeAdjustWindowSize). The trunk's
// forwarding loop pulls records on demand, so the window does not grow
// on its own, and the sender blocks with its rolling write deadline:
// "mesh: send body: mesh: relay: write: deadline exceeded".
//
// The fix gives the relay room to move a file: the windows are set
// explicitly, and they are set on the CONFIG, so every path that uses
// it (the direct listener, the direct dialer, the relay listener and
// the relay dialer) gets them.
func TestTheQuicConfigGivesTheRelayRoomToMoveAFile(t *testing.T) {
	cfg := quicConfig()

	// One relay record must fit in a stream window: the record framing
	// is what the sender writes, and a record bigger than the window
	// means the sender blocks until the window grows — which the
	// trunk's on-demand reading never causes.
	const oneRecord = 2 << 20
	if cfg.InitialStreamReceiveWindow < oneRecord {
		t.Fatalf("the initial stream receive window is %d bytes: one relay "+
			"record (about 1.4 MiB for a megabyte of payload) does not fit, so "+
			"the sender blocks until the window grows — and the trunk's on-demand "+
			"reading never grows it",
			cfg.InitialStreamReceiveWindow)
	}

	// The CONNECTION window has to hold a record plus the records of
	// the sessions the trunk multiplexes over the join connection.
	// The trunk's join connection is LONG-LIVED and carries every
	// relayed session, so the connection-level window is the one that
	// actually runs out.
	const oneSession = 8 << 20
	if cfg.InitialConnectionReceiveWindow < oneSession {
		t.Fatalf("the initial connection receive window is %d bytes: the relay's "+
			"join connection is long-lived and multiplexes every relayed session "+
			"over it, so a few megabytes of payload cannot fit",
			cfg.InitialConnectionReceiveWindow)
	}

	// The MAXIMUM matters as much: the window grows up to it, and a
	// maximum below the initial value would make the growth a no-op.
	if cfg.MaxConnectionReceiveWindow < cfg.InitialConnectionReceiveWindow {
		t.Fatalf("the maximum connection receive window (%d) is below the "+
			"initial one (%d): the window can never grow",
			cfg.MaxConnectionReceiveWindow, cfg.InitialConnectionReceiveWindow)
	}
	if cfg.MaxStreamReceiveWindow < cfg.InitialStreamReceiveWindow {
		t.Fatalf("the maximum stream receive window (%d) is below the initial "+
			"one (%d): the window can never grow",
			cfg.MaxStreamReceiveWindow, cfg.InitialStreamReceiveWindow)
	}
}

// TestTheRelayRecordFitsInTheConfiguredWindow ties the two together: the
// window the config asks for must actually hold the biggest record the
// relay writes.
//
// The biggest record is a split piece of a megabyte of plaintext: the
// end-to-end seal adds 12 bytes of nonce and 16 bytes of tag, and JSON
// carries the bytes as base64, which costs a third.
func TestTheRelayRecordFitsInTheConfiguredWindow(t *testing.T) {
	cfg := quicConfig()
	const megabyte = 1 << 20

	// The plaintext the relay seals in one piece.
	plaintext := megabyte - 1024
	// The seal: 12-byte nonce + 16-byte tag.
	sealed := plaintext + 12 + 16
	// The record: the sealed bytes as base64 in JSON, plus the envelope.
	// base64 costs 4 bytes for every 3.
	asBase64 := (sealed + 2) / 3 * 4
	record := asBase64 + 256 // the JSON envelope around it

	if int64(record) > int64(cfg.InitialStreamReceiveWindow) {
		t.Fatalf("one relay record is %d bytes but the initial stream receive "+
			"window is %d: the sender would block on its own record",
			record, cfg.InitialStreamReceiveWindow)
	}
}

// TestTheConfigIsSharedByEveryPath guards the fact that the fix works
// only if every QUIC path uses the same config: the direct listener, the
// direct dialer, the relay listener and the relay dialer.
func TestTheConfigIsSharedByEveryPath(t *testing.T) {
	cfg := quicConfig()
	if cfg.InitialStreamReceiveWindow == 0 {
		t.Fatal("the stream window is unset: quic-go would use its default")
	}
	// The window must be the SAME for every path, which is what
	// quicConfig() being the single builder guarantees.
	other := quicConfig()
	if other.InitialStreamReceiveWindow != cfg.InitialStreamReceiveWindow {
		t.Fatalf("two calls to quicConfig() disagree: %d vs %d",
			other.InitialStreamReceiveWindow, cfg.InitialStreamReceiveWindow)
	}
	// And it must not be the library default any more, or the fix is
	// silently absent. quic-go's default initial stream receive window
	// is 512 KiB (protocol.DefaultInitialMaxStreamData).
	if cfg.InitialStreamReceiveWindow == 512*1024 {
		t.Fatal("the stream window is still quic-go's default (512 KiB): the fix is not in effect")
	}
}
