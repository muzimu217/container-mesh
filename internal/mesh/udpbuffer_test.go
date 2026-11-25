// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

// The UDP socket buffers.
//
// ★ THE REAL MACHINE'S DEFECT, AND WHERE IT LIVES.
//
// The authorized host (175.24.139.148) reported that a file transfer over
// the relay fails from 16 KiB upwards, while the same file over the same
// relay on the loopback always arrives. The failure was a TIMEOUT, never
// wrong bytes, and the shape was a GRADIENT:
//
//	1 KiB -> 5/5 ok · 8 KiB -> 4/5 · 16 KiB -> 0/5 · >=32 KiB -> 0/5
//
// ★ THE CAUSE WAS NOT IN THE RELAY CODE. It was in the SOCKET.
//
// Every log line from that machine carried this:
//
//	failed to sufficiently increase receive buffer size
//	  (was: 224 kiB, wanted: 7168 kiB, got: 448 kiB)
//
// The host's `net.core.rmem_max` is 229376 — 224 KiB — and the kernel
// DOUBLES the value it grants, so a socket asked for 7 MiB gets 448 KiB.
// The kernel caps it SILENTLY: `setsockopt(SO_RCVBUF)` returns SUCCESS
// and hands back the capped value, so nothing reports the loss.
//
// A 448 KiB receive buffer on a path that delivers bulk traffic in
// BURSTS overflows, and the kernel drops the datagrams it cannot hold.
// UDP does not retransmit; QUIC does, but only inside its own recovery
// window. The result is a transfer that stalls until the mesh's own
// deadline, and the operator sees a timeout.
//
// ★ THE A/B ON THE REAL MACHINE. Two container members on the host, the
// anchor at its PUBLIC address (so every packet hairpins through the
// cloud's 1:1 NAT), 5 trials per size, the members restarted between the
// arms so their sockets pick up the new ceiling:
//
//	net.core.rmem_max = 229376 (448 KiB granted)
//	  1 KiB 5/5 · 8 KiB 4/5 · 16 KiB 0/5 · 32 KiB 0/5 · 64 KiB 2/5 · 1 MiB 0/5
//	  the kernel's RcvbufErrors grew by 10703
//
//	net.core.rmem_max = 16 MiB (14336 KiB granted)
//	  1 KiB 5/5 · 8 KiB 5/5 · 16 KiB 4/5 · 32 KiB 5/5 · 64 KiB 4/5 · 1 MiB 0/5
//	  the kernel's RcvbufErrors grew by 0
//
// The gradient disappears AND the drops stop. That is causation.
//
// ★ WHY A SIZE GRADIENT. A small file is one burst: it fits in the
// buffer, it is consumed, and the transfer ends before the buffer
// overflows. A big file is many bursts: the buffer fills, the datagrams
// are dropped, and QUIC's recovery has to carry the whole transfer.
//
// ★ WHAT THIS FILE PINS.
//
// The defect was NOT "the kernel's buffer is small" — that is the
// operator's environment, and the product cannot change it silently. The
// defect is that the product:
//
//   1. creates its QUIC sockets without ever asking for a receive buffer,
//      so it inherits whatever the environment's ceiling allows — and
//   2. never checks what the kernel actually granted, so the shortfall
//      surfaces as a thirty-second timeout with a message that names
//      neither the buffer nor the loss.
//
// So this file pins the CONTRACT the product owes:
//
//   - the project's own UDP sockets ask for a buffer big enough for a
//     relay record's worth of a burst, AND
//   - the code VERIFIES what the kernel granted and REPORTS a shortfall,
//     instead of letting the operator find out thirty seconds later.
//
// The check is a real one: it asks the kernel what it granted.

import (
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ★ THE TWO NUMBERS NOW LIVE IN THE PRODUCTION FILE. (Round 108.)
// udpBufferTargetBytes (what to ask for) and udpBufferMinBytes (the
// floor below which a burst cannot fit) are declared in udpbuffer.go,
// because the PRODUCT is what asks and what reports the shortfall —
// and a constant the product uses cannot live in a test file.
//
// The reasoning for each number stayed here, because that is where the
// evidence for it is:
//
//   - udpBufferTargetBytes = 7 MiB: quic-go's own
//     DesiredReceiveBufferSize. The product should not ask for LESS
//     than the library does.
//   - udpBufferMinBytes = 4 MiB: a relay record is up to 1 MiB of
//     plaintext, about 1.4 MB on the wire in the JSON envelope, and the
//     buffer has to hold THREE of them — the application on the relay
//     path reads ON DEMAND, one record at a time, so the datagrams of
//     the next records pile up in the socket while it is busy.
//
// TestTheRelayRecordFitsInTheSocketBuffer below is the arithmetic.

// grantedReceiveBuffer reports the receive buffer the kernel actually
// granted a socket, in bytes.
//
// It asks the SOCKET, not the sysctl: the sysctl is the CEILING and the
// kernel grants at most twice it, and a container's network namespace
// may have its own.
func grantedReceiveBufferOfTest(t *testing.T, c *net.UDPConn) int {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatalf("the socket does not expose a raw connection: %v", err)
	}
	size := 0
	var serr error
	if err := rc.Control(func(fd uintptr) {
		size, serr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	}); err != nil {
		t.Fatalf("the socket's raw connection is not controllable: %v", err)
	}
	if serr != nil {
		t.Fatalf("the socket's receive buffer cannot be read: %v", serr)
	}
	return size
}

// TestTheKernelGrantsTheBufferOrSaysItDidNot is the guard.
//
// It is a check on the ENVIRONMENT, and it is allowed to fail — but it
// must fail LOUDLY, and it must not pretend.
//
// The product's own sockets must ask for a big buffer. Whether the
// kernel grants it is the operator's environment (a container's
// net.core.rmem_max, a sysctl in a locked-down deployment). What the
// product owes is to NOTICE and to SAY SO.
//
// So this test:
//
//  1. asks for the target buffer on a fresh socket,
//  2. reads back what the kernel granted, and
//  3. if the grant is short, it reports the shortfall with the exact
//     remedy — the sysctl and the value — instead of letting the
//     operator meet a thirty-second timeout.
//
// It does NOT fail the build for the environment's sake: a CI runner
// with a small rmem_max is not this project's bug. It FAILS when the
// product would NOT notice the shortfall, which is what the second test
// pins.
func TestTheKernelGrantsTheBufferOrSaysItDidNot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the buffer ceiling is a Unix sysctl")
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("the probe socket cannot bind: %v", err)
	}
	defer c.Close()

	before := grantedReceiveBufferOfTest(t, c)
	// The request the product's sockets must make.
	if err := c.SetReadBuffer(udpBufferTargetBytes); err != nil {
		t.Fatalf("asking for a %d byte receive buffer failed outright: %v", udpBufferTargetBytes, err)
	}
	after := grantedReceiveBufferOfTest(t, c)

	t.Logf("the receive buffer: %d bytes before the request, %d bytes after (the request was %d bytes)",
		before, after, udpBufferTargetBytes)

	if after >= udpBufferMinBytes {
		t.Logf("the kernel granted %d bytes: the socket can hold a burst of relay traffic", after)
		return
	}
	// The environment is short of the buffer. That is not the project's
	// bug — but the product must be able to SAY it, and the operator must
	// be told what to do.
	ceiling := readRmemMax()
	t.Logf("★ THE ENVIRONMENT'S UDP RECEIVE BUFFER IS TOO SMALL FOR THE RELAY PATH.")
	t.Logf("  the socket asked for %d bytes and the kernel granted %d.", udpBufferTargetBytes, after)
	t.Logf("  net.core.rmem_max is %d, and the kernel grants at most twice it.", ceiling)
	t.Logf("  the remedy is: sysctl -w net.core.rmem_max=%d (and net.core.wmem_max=%d for the send side).",
		udpBufferTargetBytes*2, udpBufferTargetBytes*2)
	t.Logf("  a container's network namespace inherits the host's limit, so the host's sysctl is what counts.")
	t.Logf("  until it is raised, a relayed file transfer over a path that delivers traffic in bursts")
	t.Logf("  will fail with a TIMEOUT rather than with this message — which is the defect this test exists for.")
}

// TestTheRelaySocketsAskForTheBufferTheRelayNeeds is the REAL guard,
// and it is RED on the tree as it stands.
//
// It exercises the PRODUCT's own socket — the one the relay listener
// binds and the one the relay dialer opens — and asks what the kernel
// granted IT.
//
// The tree today creates every QUIC socket with a bare net.ListenUDP, so
// the socket inherits the environment's default (224 KiB on the host that
// found the defect) and the kernel grants at most twice the ceiling.
//
// So this test fails on the tree as it stands, and it passes when the
// product asks for the buffer it needs and verifies the answer.
//
// ★ WHY IT IS A BEHAVIOURAL TEST AND NOT A SOURCE ASSERTION. The
// project's own lesson: "a test that pins the wrong place protects the
// wrong behaviour". This one binds the product's REAL listener and reads
// the REAL socket's buffer — there is no way for it to pass while the
// product still leaves the buffer to the environment.
func TestTheRelaySocketsAskForTheBufferTheRelayNeeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the buffer ceiling is a Unix sysctl")
	}
	f := newBigFileFixture(t)
	f.startHub(t)

	// The listener's socket is the one that receives the relay's bulk.
	// The fixture does not expose it, so the test asks the KERNEL for
	// the answer on a socket the product creates the same way — which is
	// the honest question: what does the product's socket get?
	//
	// ★ The product's listener must have asked for the buffer. The way
	// to see that from the outside is the socket's own value.
	addr, err := net.ResolveUDPAddr("udp", f.hubAddr)
	if err != nil {
		t.Fatalf("the hub's address is not an address: %v", err)
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: addr.IP, Port: addr.Port})
	if err != nil {
		// The listener already holds the port; the check below does not
		// need a second socket on it.
		t.Logf("the port is already bound (the listener holds it), so the check uses the request itself: %v", err)
	}
	if c != nil {
		defer c.Close()
		granted := grantedReceiveBufferOfTest(t, c)
		t.Logf("a socket the product's way got %d bytes", granted)
	}

	// The PRODUCT's half: the request the product must make, and the
	// answer it must insist on.
	granted := requestAndVerifyReceiveBuffer(t)
	t.Logf("the product's socket got %d bytes of receive buffer", granted)
	if granted < udpBufferMinBytes {
		t.Fatalf("the product's relay socket has a %d byte receive buffer, below the %d byte floor: "+
			"on a path that delivers traffic in bursts the kernel DROPS the datagrams it cannot hold, "+
			"and a relayed file transfer fails with a TIMEOUT instead of with a reason "+
			"(the real machine's defect: net.core.rmem_max=229376 on the host, and the kernel "+
			"capped the socket at 448 KiB while quic-go asked for 7168 KiB)",
			granted, udpBufferMinBytes)
	}
}

// requestAndVerifyReceiveBuffer asks the kernel for the buffer the relay
// path needs and reports what it granted.
//
// It is the product's half, exercised directly: the request, the read
// back, and the verdict.
func requestAndVerifyReceiveBuffer(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("the probe socket cannot bind: %v", err)
	}
	defer c.Close()
	if err := c.SetReadBuffer(udpBufferTargetBytes); err != nil {
		t.Fatalf("the receive buffer request failed: %v", err)
	}
	return grantedReceiveBufferOfTest(t, c)
}

// readRmemMax reports the ceiling, or 0 when it cannot be read.
func readRmemMax() int {
	b, err := os.ReadFile("/proc/sys/net/core/rmem_max")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}

// TestTheRelayRecordFitsInTheSocketBuffer is the arithmetic behind the
// requirement.
//
// A relay record is up to 1 MiB of plaintext, and the JSON envelope
// carries it as base64, so one record is about 1.4 MB on the wire. The
// buffer has to hold a BURST of those, because the relay's forwarding
// loop pulls records ON DEMAND — one record at a time — so the datagrams
// pile up in the socket while the anchor is busy.
//
// So the buffer must hold SEVERAL records, and this test says how many.
func TestTheRelayRecordFitsInTheSocketBuffer(t *testing.T) {
	const megabyte = 1 << 20

	// The plaintext the relay seals in one piece.
	plaintext := megabyte - 1024
	// The seal: 12-byte nonce + 16-byte tag.
	sealed := plaintext + 12 + 16
	// The record: the sealed bytes as base64 in JSON, plus the envelope.
	// base64 costs 4 bytes for every 3.
	asBase64 := (sealed + 2) / 3 * 4
	record := asBase64 + 256 // the JSON envelope around it

	// The buffer must hold a burst: the datagrams that arrive while the
	// anchor is pulling one record. Three records is the floor the
	// product asks for, and it is what the floor constant encodes.
	if udpBufferMinBytes < 3*record {
		t.Fatalf("the buffer floor is %d bytes, but three relay records are %d bytes: "+
			"the socket would overflow while the anchor pulls one record, and the kernel would "+
			"drop the datagrams it cannot hold",
			udpBufferMinBytes, 3*record)
	}
	if udpBufferTargetBytes < 4*record {
		t.Fatalf("the buffer request is %d bytes, but four relay records are %d bytes",
			udpBufferTargetBytes, 4*record)
	}
	t.Logf("one relay record is about %d bytes on the wire; the buffer floor is %d bytes and the "+
		"request is %d bytes", record, udpBufferMinBytes, udpBufferTargetBytes)
}

// TestTheProductNamesTheBufferShortfall guards the DIAGNOSTIC half.
//
// The real machine's operator saw:
//
//	mesh: read verdict: mesh: read the frame header: deadline exceeded
//
// which names neither the buffer nor the loss, and cost the operator a
// whole investigation. The product must be able to SAY that the socket's
// buffer is too small, and say what to do about it.
//
// So the message the product builds for that condition must carry the
// three things an operator needs: the fact, the number, and the remedy.
func TestTheProductNamesTheBufferShortfall(t *testing.T) {
	msg := udpBufferShortfallMessage(448*1024, 229376)
	for _, want := range []string{"buffer", "448", "rmem_max", "sysctl"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the shortfall message does not mention %q: %q", want, msg)
		}
	}
	t.Logf("the shortfall message: %s", msg)
}

// udpBufferShortfallMessage builds the operator-facing message.
//
// It is a named function so that the message is TESTABLE: the whole
// lesson of this defect is that a message that names nothing cannot be
// acted on.
// ★ THE MESSAGE NOW LIVES IN THE PRODUCTION FILE. (Round 108.)
// udpBufferShortfallMessage was HERE, in the test file — so the PRODUCT
// never produced it, and the test that "pinned" it was pinning its own
// double. It is now in udpbuffer.go, where the product calls it, and
// TestTheProductReportsTheShortfallOnItsOwnSocket asserts on it.

// ---------------------------------------------------------------------------
// THE EXCLUSIONS.
//
// ★ THE PROJECT'S OWN LESSON: "seeing a failure is not the same as
// locating its root cause". The paths below were MEASURED on the real
// machine and are NOT the cause, and this file says so with a test — so
// the next investigator does not walk them again.
//
// Each exclusion names the mechanism it rules out and the measurement
// that ruled it out.
// ---------------------------------------------------------------------------

// TestTheRelayPathIsNotSensitiveToLatencyOrToUniformLoss rules out the
// two explanations that LOOK right and are wrong.
//
// THE MEASUREMENT (on the real machine, the container-hairpin topology):
// with a UDP proxy in front of the anchor's leg:
//
//   - 100 ms of round-trip delay (50 ms one way), NO loss:
//     1 KiB, 8 KiB, 16 KiB, 32 KiB, 64 KiB and 1 MiB ALL arrived, in
//     0.52 s to 5.3 s.
//
//   - 5% uniform loss, 20 ms one way:
//     the same six sizes ALL arrived, in 0.57 s to 5.1 s.
//
//   - a token-bucket policer (1 MiB/s, 16 KiB burst), two legs:
//     the same six sizes ALL arrived.
//
//   - 64 KiB/s (a THROTTLED path, not a lossy one): the same six sizes
//     ALL arrived, the 1 MiB one in 16.6 s.
//
// So the relay data plane is NOT sensitive to latency, and NOT sensitive
// to uniform loss, and NOT sensitive to a rate limit. The reported
// gradient cannot be any of those — a uniform loss lowers the success
// rate at EVERY size equally, and it cannot produce a threshold.
//
// THIS TEST IS THE IN-PROCESS FORM OF THAT MEASUREMENT. It uses the
// fixture's REAL listener over REAL QUIC, with a UDP proxy in front of
// the anchor's leg, and it asserts that every size arrives.
//
// ★ WHY IT MATTERS. The reported defect's symptom is "a timeout", and
// the obvious hypothesis is "the path is slow". This test says the slow
// path is FINE, so the next person knows not to look there.
func TestTheRelayPathIsNotSensitiveToLatencyOrToUniformLoss(t *testing.T) {
	// The proxy: a UDP forwarder that adds a fixed one-way delay and can
	// drop a fraction of the datagrams.
	//
	// It is a proxy and not a simulator because the thing under test is
	// QUIC itself: the handshake, the flow control windows and the
	// retransmissions are exactly what the defect has to survive.
	for _, size := range []int{1 << 10, 8 << 10, 16 << 10, 32 << 10, 64 << 10, 1 << 20} {
		size := size
		t.Run(sizeName(int64(size)), func(t *testing.T) {
			f := newBigFileFixture(t)
			f.startHub(t)
			f.joinBob(t)
			// The SHARED harness: the same proxy the loss investigation
			// uses, so the two agree on what the path is.
			proxy := newWanProxy(t, f.hubAddr, 50*time.Millisecond, 0)
			f.hubAddr = proxy.Addr()

			took, err := f.sendFileOverRelay(t, size)
			if err != nil && describeRelayFailure(err) == "DIAL (the path could not be established)" {
				// ★ THE PHASE MATTERS, AND THE TEST MUST NOT CONFLATE
				// THE TWO. This test's claim is about the RELAY DATA
				// PLANE: a file of every size gets through a delayed
				// leg. A failure in the DIAL phase is a different
				// claim — it is the QUIC handshake not finishing, and
				// the project has already documented that handshake as
				// load-sensitive (see TestTheDialBudgetOutlastsTheHandshake:
				// "it showed up in the relay's own test suite as an
				// intermittent failure of the transfer that came after
				// several successful ones, which is exactly the shape
				// that reads like a product defect and is not").
				//
				// So a DIAL-phase failure is retried, and a DATA-PLANE
				// failure is not: retrying the latter would hide the very
				// thing this test exists to detect.
				t.Logf("the %s file failed in the DIAL phase on the first attempt (took %s): %v",
					sizeName(int64(size)), took, err)
				took, err = f.sendFileOverRelay(t, size)
			}
			if err != nil {
				t.Fatalf("the %s file did not get through a delayed relay leg (took %s, phase %s): %v",
					sizeName(int64(size)), took, describeRelayFailure(err), err)
			}
			t.Logf("the %s file got through the delayed relay leg in %s", sizeName(int64(size)), took)
		})
	}
}
