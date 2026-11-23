// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// ---------------------------------------------------------------------------
// The BYTE SPLICE.
//
// THIS IS THE SECOND STEP of the binary relay record (relayframe.go). Step
// one defined the format and its codec; step two is the FORWARDING PATH
// that the format exists for: the anchor copies a record from one leg of
// the relay to the other WITHOUT DECODING IT.
//
// WHY THE ANCHOR MUST NOT DECODE.
//
// The relay trunk sits between the requester (A) and the target (C) and
// holds NO KEY for either of them. The end-to-end seal (SIGMA + AES-256-GCM,
// session.go) lives ABOVE the record layer, so the bytes the anchor carries
// are ciphertext and the anchor cannot open them. What the anchor NEEDS
// from a record is only: what kind of record it is, how big it is, and how
// many payload bytes to charge against the rate budget.
//
// The JSON envelope could not give it that cheaply. JSON encodes a []byte
// as base64, so the anchor had to decode every record it forwarded
// (json.Unmarshal + base64 decode) and re-encode it (json.Marshal +
// base64 encode) — 36.27% of the anchor's CPU in the measured profile
// (base64 21.69% of it), and 27.76% of the target endpoint's CPU too. The
// binary frame puts the type, the meta length and the payload length in a
// FIXED 24-byte header, so the anchor can learn everything it needs from
// 24 bytes and then copy the rest.
//
// THE SHAPE OF A RELAY.
//
// A relay is TWO legs: A↔B and B↔C. The record format is an A↔C
// PROTOCOL — the anchor is a pipe. So the two legs MUST agree on the
// format: if the requester writes binary frames and the target expects
// JSON, a verbatim splice hands the target garbage. This is why the
// splicer refuses to be built for two legs that disagree (see
// NewRelaySplicer) and why it refuses a frame it cannot understand at
// RUNTIME (see ForwardOne).
//
// WHAT THE SPLICE DOES NOT DO.
//
// It does not decode the payload, it does not look at the meta block
// beyond the lengths the header declares, and it does not decide what a
// record MEANS beyond the type byte in the header. The one thing it does
// with the type byte is recognise the CLOSE record, because that record
// ends the direction and the far side has to learn about it.
//
// THIS FILE IS THE FAST PATH, AND IT IS LIVE.
//
// The trunk's forwarding path uses it whenever the relay's two legs both
// speak the binary frame — which is the DEFAULT deployment, and the one
// the whole switch is about. It exists as a SEPARATE unit on purpose:
// the compatibility properties (the splice refuses to be built for two
// legs that disagree; it refuses a frame it cannot understand at
// RUNTIME) are properties of a unit that can be proved on their own,
// rather than of a forwarding loop that has a dozen other concerns.
//
// See docs/design/RELAY-FRAME.md.
// ---------------------------------------------------------------------------

// RelaySpliceFormat names the record format of ONE leg of a relay.
//
// A leg's format is a property of the CONNECTION it rides on (it comes
// from the negotiated ALPN), not of the record: the same relay carries
// records in one format in one direction and in another format in the
// other direction only if the two endpoints disagree, which is a
// misconfiguration and not a feature.
type RelaySpliceFormat uint8

const (
	// RelaySpliceFormatBinary is the binary relay frame: a 24-byte fixed
	// header followed by a meta block and an opaque payload.
	RelaySpliceFormatBinary RelaySpliceFormat = iota
	// RelaySpliceFormatJSON is the length-prefixed JSON envelope that the
	// trunk speaks today.
	RelaySpliceFormatJSON
)

// String names the format the way the error messages name it.
func (f RelaySpliceFormat) String() string {
	switch f {
	case RelaySpliceFormatBinary:
		return "binary"
	case RelaySpliceFormatJSON:
		return "JSON"
	default:
		return fmt.Sprintf("unknown relay format (%d)", uint8(f))
	}
}

// The splicer's errors.
//
// They are sentinels so that the caller can tell the three failures apart
// WITHOUT reading the message:
//
//   - ErrRelaySpliceFormat: the bytes are not a binary relay frame. The
//     peer speaks the other format, or the record is corrupt. Either way
//     it must NOT be forwarded.
//   - ErrRelaySpliceTruncated: the stream ended in the middle of a
//     record. NOT a clean end — see the note on io.EOF below.
//   - ErrRelaySpliceTooLarge: the record is past the relay record cap.
//   - ErrRelaySpliceMismatch: the two legs of the relay disagree about
//     the record format, so the anchor would have to translate (and
//     translating means decoding).
//   - ErrRelaySpliceUnsupported: there is nothing to splice — both legs
//     already speak a format the trunk can handle without this unit.
//   - ErrRelaySpliceFlowEnded: the relay flow ended while the splice was
//     waiting for the rate budget.
var (
	// ErrRelaySpliceFormat says the bytes are not a binary relay frame.
	ErrRelaySpliceFormat = errors.New("mesh: relay: the record is not a binary relay frame")
	// ErrRelaySpliceTruncated says the stream ended inside a record.
	ErrRelaySpliceTruncated = errors.New("mesh: relay: the record was truncated")
	// ErrRelaySpliceTooLarge says the record is past the record cap.
	ErrRelaySpliceTooLarge = errors.New("mesh: relay: the record is past the relay record cap")
	// ErrRelaySpliceMismatch says the two legs disagree about the format.
	ErrRelaySpliceMismatch = errors.New("mesh: relay: the two legs of the relay disagree about the record format")
	// ErrRelaySpliceUnsupported says there is nothing to splice.
	ErrRelaySpliceUnsupported = errors.New("mesh: relay: there is nothing to splice")
	// ErrRelaySpliceFlowEnded says the flow ended under the splice.
	ErrRelaySpliceFlowEnded = errors.New("mesh: relay: the relay flow ended")
)

// RelaySpliceQuota is the rate budget the splice charges against.
//
// The budget is charged on the PAYLOAD — the bytes the anchor actually
// carries — and NOT on the envelope around it. The JSON path charged
// len(rec.Data); the binary header carries the same number, so the
// charge is the same number and it needs no decoding.
//
// The shape of the interface mirrors the trunk's own chargeBytes: the
// call reports how long the caller must WAIT before the bytes may be
// forwarded. A zero wait means the bytes are budgeted now. A positive
// wait means the bucket was dry and the bytes were NOT charged yet: the
// caller waits and asks again, because the refill may have been drunk by
// the flow's other direction in the meantime.
type RelaySpliceQuota interface {
	// Reserve charges payloadLen payload bytes against the budget and
	// reports how long the caller must wait before they may move. A zero
	// wait means the charge is done.
	Reserve(payloadLen int64) (wait time.Duration, err error)
}

// relaySpliceChunk is the size of the splice's scratch buffer.
//
// The buffer is REUSED for every record, so the splice allocates nothing
// per record — which is the whole point of the fast path: the JSON path
// allocated 83 KB per 32 KiB record, and the measured relay throughput
// ceiling is set by the anchor's single saturated core.
//
// 32 KiB is a compromise: big enough that the per-write overhead is
// negligible, small enough that a splicer is cheap to hold.
const relaySpliceChunk = 32 << 10

// RelaySplicer forwards binary relay records from one leg of a relay to
// the other, byte for byte.
//
// It is a SELF-CONTAINED unit: it starts no goroutine, it opens no
// session, and it closes only the session it is given. The relay's own
// forwarding goroutine calls it, so there is nothing to join and nothing
// to leak.
//
// It is safe for concurrent use: the scratch buffer and the head buffer
// are guarded, so one splicer can serve both directions of a relay. (The
// trunk gives each direction its own splicer in practice, but the unit
// does not depend on that.)
type RelaySplicer struct {
	// NearLeg is the format the reading leg speaks.
	NearLeg RelaySpliceFormat
	// FarLeg is the format the writing leg speaks.
	FarLeg RelaySpliceFormat
	// Quota is the rate budget. It may be nil, which means "no budget".
	Quota RelaySpliceQuota
	// Done is closed when the relay flow ends. A splice that is waiting
	// for the rate budget stops waiting when it is closed, so a flow that
	// ends does not keep its forwarding goroutine (and its session slot)
	// for as long as the wait would have lasted. It may be nil.
	Done <-chan struct{}

	mu sync.Mutex
	// head holds the length prefix and the fixed header of the record
	// being forwarded: 4 + 24 bytes.
	head [4 + relayFrameHeaderSize]byte
	// scratch is the copy buffer for the record's body.
	scratch []byte
}

// NewRelaySplicer builds a splicer for a relay whose two legs speak the
// given formats.
//
// It REFUSES two shapes:
//
//   - the legs DISAGREE. The record format is a protocol between the two
//     ENDPOINTS; the anchor is a pipe. Translating between formats means
//     DECODING, which is the thing the splice exists to avoid, so the
//     splice does not pretend it can do it. The caller falls back to the
//     JSON path (or the mixed path) instead.
//   - both legs speak JSON. There is nothing to splice: the JSON path
//     already works, and a second implementation of the same wire format
//     is a second thing to keep correct.
func NewRelaySplicer(near, far RelaySpliceFormat) (*RelaySplicer, error) {
	if near != far {
		return nil, fmt.Errorf("%w: the requester's leg speaks %s and the target's leg speaks %s; "+
			"the anchor would have to translate, and translating means decoding",
			ErrRelaySpliceMismatch, near, far)
	}
	if near != RelaySpliceFormatBinary {
		return nil, fmt.Errorf("%w: both legs speak %s; the trunk already forwards that format without this unit",
			ErrRelaySpliceUnsupported, near)
	}
	s := &RelaySplicer{
		NearLeg: near,
		FarLeg:  far,
		scratch: make([]byte, relaySpliceChunk),
	}
	return s, nil
}

// RelaySpliceFormatOf says which record format a record body is in.
//
// It looks at the FIRST BYTE only, and that is enough: the binary frame
// starts with the format tag 0xC7, which is not a byte any JSON parser
// can start a document with, and a JSON envelope starts with '{'.
//
// It exists because the anchor needs to know a leg's format BEFORE it
// commits to a forwarding path, and because a splice that cannot tell
// the two formats apart would forward a mismatched frame.
func RelaySpliceFormatOf(body []byte) (RelaySpliceFormat, error) {
	if len(body) == 0 {
		return 0, fmt.Errorf("%w: the record body is empty", ErrRelaySpliceFormat)
	}
	switch body[0] {
	case relayFrameMagic:
		return RelaySpliceFormatBinary, nil
	case '{':
		return RelaySpliceFormatJSON, nil
	default:
		return 0, fmt.Errorf("%w: bad magic byte 0x%02x (the binary relay frame tag is 0x%02x, a JSON envelope starts with '{')",
			ErrRelaySpliceFormat, body[0], relayFrameMagic)
	}
}

// RelayALPN2 is the ALPN of the binary relay record format.
//
// It is the sibling of RelayALPN ("container-mesh-relay/1", relaytrunk.go)
// and it is defined HERE rather than there because this file is the one
// that knows what a record format IS.
//
// THE ORDER MATTERS. Go's ALPN negotiation is SERVER-FIRST: it walks the
// SERVER's list in order and takes the first entry the client also
// offers (measured under QUIC in TestTheALPNNegotiationUnderQUICIsServerFirst,
// and over crypto/tls in docs/design/RELAY-FRAME.md §4.3). So the anchor
// must advertise [RelayALPN2, RelayALPN] — the NEW one FIRST — or it
// negotiates the old protocol with a member that could have spoken the
// new one. See relayAdvertisedALPNs.
//
// The member must offer BOTH. A member that offers only RelayALPN2
// cannot connect to an old anchor at all: the handshake fails with "no
// application protocol" (TestAClientThatOffersOnlyTheNewALPNCannotReachAnOldAnchor).
const RelayALPN2 = "container-mesh-relay/2"

// RelaySpliceFormatForALPN says which record format a connection that
// negotiated the given ALPN speaks.
//
// The ALPN is the version negotiation, and it is the ONLY one an old peer
// takes part in: an old peer does not sniff records, it only has the
// format its code knows. So the mapping is the whole compatibility
// mechanism:
//
//   - container-mesh-relay/2 → the binary relay frame.
//   - container-mesh-relay/1 → the JSON envelope.
//
// An ALPN this code does not know is a REFUSAL, not a guess: the
// negotiated protocol names a record format this anchor cannot speak, and
// guessing would put the anchor on a forwarding path the peer is not on.
//
// The anchor advertises the new ALPN FIRST (Go walks the server's list
// in order) and the member offers BOTH (or the member cannot reach an
// old anchor at all). See relayAdvertisedALPNs, and
// docs/design/RELAY-FRAME.md §4.3 and §4.5.
func RelaySpliceFormatForALPN(alpn string) (RelaySpliceFormat, error) {
	switch alpn {
	case RelayALPN2:
		return RelaySpliceFormatBinary, nil
	case RelayALPN:
		return RelaySpliceFormatJSON, nil
	default:
		return 0, fmt.Errorf("%w: mesh: the negotiated ALPN %q names no record format this anchor speaks",
			ErrRelaySpliceUnsupported, alpn)
	}
}

// PeekRelaySpliceFormat says which record format the NEXT record on a
// leg is in, without consuming it.
//
// The record's first byte sits BEHIND the framing layer's length prefix,
// so the peek is of prefix+body. It needs five bytes.
//
// It is the pre-flight check: an anchor that sniffs both legs before it
// starts splicing knows whether the two legs agree, and it knows it
// BEFORE the first record has been forwarded — which is the difference
// between a clean refusal and a half-forwarded garbage record.
func PeekRelaySpliceFormat(br *bufio.Reader) (RelaySpliceFormat, error) {
	head, err := br.Peek(4 + 1)
	if err != nil {
		if len(head) >= 5 {
			return 0, fmt.Errorf("%w: mesh: the record header is truncated: %v", ErrRelaySpliceTruncated, err)
		}
		if len(head) == 0 {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("%w: mesh: %d of 5 header bytes arrived: %v", ErrRelaySpliceTruncated, len(head), err)
	}
	return RelaySpliceFormatOf(head[4:])
}

// Forward forwards records from br to to until the leg ends.
//
// It is the LOOP: the unit the relay's forwarding goroutine runs. The
// three ways it ends:
//
//   - the peer half-closes BETWEEN records: a clean end. The far side is
//     HALF-CLOSED (not aborted) — it may still have an answer on the way,
//     which is what every request/response protocol does.
//   - the peer sends a CLOSE record: the close is forwarded (the far side
//     has to learn about it) and the direction ends with a half close.
//   - anything else: the stream is BROKEN. The far side is ABORTED, so it
//     sees a reset rather than a finish. Half-closing there would tell
//     the far side "the relay finished", and the far side would believe
//     the tail of the record was the end of the message.
//
// The truncation error deliberately does NOT satisfy errors.Is(err,
// io.EOF). Every "EOF means the sender finished" caller in this package
// would mistake a truncation for a graceful finish, and the tail of the
// record would be lost silently. That is the same lesson the message
// path learned (see errTruncatedFrame in direct.go).
func (s *RelaySplicer) Forward(br *bufio.Reader, to Session) error {
	for {
		hdr, err := s.ForwardOne(br, to)
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = to.Close()
				return nil
			}
			_ = abortSession(to)
			return err
		}
		if hdr.Type == uint8(wire.FrameMeshRelayClose) {
			// The peer is done. The close record has been forwarded
			// already, so the far side knows. This direction ends with a
			// HALF close: the far side may still have something to say.
			_ = to.Close()
			return nil
		}
	}
}

// ForwardOne forwards ONE record from br to to and reports its header.
//
// It is the primitive under Forward, and it is where the interesting
// decisions live:
//
//   - the record's length comes from the FIXED HEADER, not from a scan:
//     the header says how long the meta block and the payload are, so
//     the anchor knows how many bytes to copy before it copies the first
//     one;
//   - the length prefix and the header must AGREE. If they do not, the
//     record is malformed, and forwarding it would put the far side one
//     record out of step for the rest of the relay;
//   - the cap is checked BEFORE anything is read or written, so a peer
//     that announces a huge record cannot make the anchor wait for it;
//   - the format is checked BEFORE anything is written, so a mismatched
//     record never reaches the far side even partially.
//
// A truncation leaves the far side holding a PARTIAL record. ForwardOne
// does not tear the far side down — that is Forward's job, because
// ForwardOne is also used where the caller wants to decide — but it
// reports the truncation loudly.
func (s *RelaySplicer) ForwardOne(br *bufio.Reader, to Session) (relayFrameHeader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// ① The length prefix. A clean end is an EOF where NOTHING was read:
	// the peer half-closed at a record boundary.
	var bodyLen uint32
	if _, err := io.ReadFull(br, s.head[:4]); err != nil {
		if errors.Is(err, io.EOF) {
			return relayFrameHeader{}, io.EOF
		}
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: the length prefix is truncated: %v",
			ErrRelaySpliceTruncated, err)
	}
	bodyLen = binary.BigEndian.Uint32(s.head[:4])

	// ② The FORMAT, decided from the record's first byte.
	//
	// This comes BEFORE every other check on purpose. The first byte is
	// the only thing the splice can be certain about without parsing: it
	// says whether the peer speaks this format at all. A JSON envelope
	// starts with '{' and a binary frame starts with the format tag, so
	// one byte settles it — and settling it FIRST means the operator's
	// first error message names the format instead of naming some
	// length disagreement that is only a symptom.
	//
	// The byte is read on its own so that a body too short to hold a
	// header still gets its format identified.
	if _, err := io.ReadFull(br, s.head[4:5]); err != nil {
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: the record body is truncated: %v",
			ErrRelaySpliceTruncated, err)
	}
	switch s.head[4+relayFrameOffMagic] {
	case relayFrameMagic:
	case '{':
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: the peer is speaking the JSON relay format and this leg speaks the binary one",
			ErrRelaySpliceFormat)
	default:
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: bad magic byte 0x%02x (the binary relay frame tag is 0x%02x, a JSON envelope starts with '{')",
			ErrRelaySpliceFormat, s.head[4+relayFrameOffMagic], relayFrameMagic)
	}

	// ③ The rest of the fixed header. It is where the type, the meta
	// length and the payload length come from.
	//
	// A body that announces fewer bytes than a header is malformed, and
	// the bytes that ARE there are read so that the error can still be
	// about the record rather than about a short read.
	wantHeader := int(bodyLen)
	if wantHeader > relayFrameHeaderSize {
		wantHeader = relayFrameHeaderSize
	}
	if wantHeader > 1 {
		if _, err := io.ReadFull(br, s.head[5:4+wantHeader]); err != nil {
			return relayFrameHeader{}, fmt.Errorf("%w: mesh: the %d-byte header is truncated: %v",
				ErrRelaySpliceTruncated, relayFrameHeaderSize, err)
		}
	}
	if int64(bodyLen) < int64(relayFrameHeaderSize) {
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: the record announces %d bytes, less than the %d-byte header",
			ErrRelaySpliceFormat, bodyLen, relayFrameHeaderSize)
	}
	hdr, err := PeekRelayFrame(s.head[4:])
	if err != nil {
		return relayFrameHeader{}, fmt.Errorf("%w: %w", ErrRelaySpliceFormat, err)
	}

	// ④ The cap, on the length the HEADER declares.
	//
	// The header is where the real length lives — the prefix is only the
	// framing layer's view of it — and both come from the peer, so both
	// are checked before the anchor commits to moving that many bytes.
	total := int64(relayFrameHeaderSize) + int64(hdr.MetaLen) + int64(hdr.PayloadLen)
	if total > int64(maxRelayRecord) {
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: the record announces %d bytes and the cap is %d",
			ErrRelaySpliceTooLarge, total, maxRelayRecord)
	}
	if int64(bodyLen) > int64(maxRelayRecord) {
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: the record announces %d bytes and the cap is %d",
			ErrRelaySpliceTooLarge, bodyLen, maxRelayRecord)
	}

	// ⑤ The two lengths must agree: the prefix says how long the body
	// is, the header says how long the meta block and the payload are.
	if total != int64(bodyLen) {
		return relayFrameHeader{}, fmt.Errorf("%w: mesh: the length prefix says %d bytes and the header says %d",
			ErrRelaySpliceFormat, bodyLen, total)
	}

	// ⑥ The budget, BEFORE the write. A record that is written before it
	// is budgeted is a record that takes the node over its cap.
	if err := s.charge(int64(hdr.PayloadLen)); err != nil {
		return relayFrameHeader{}, err
	}

	// ⑦ The write. The prefix and the header go out first, then the body
	// is copied through the scratch buffer without ever being decoded.
	if err := writeFull(to, s.head[:]); err != nil {
		return relayFrameHeader{}, fmt.Errorf("mesh: relay: the spliced record could not be written: %w", err)
	}
	if err := s.copyBody(br, to, total-relayFrameHeaderSize); err != nil {
		return relayFrameHeader{}, err
	}
	return hdr, nil
}

// charge reserves the payload bytes against the rate budget and waits for
// the bucket if it is dry.
//
// The wait is interruptible: the flow's Done channel ends it. A wait that
// could not be interrupted would hold the forwarding goroutine — and the
// flow's session slot — for as long as the wait lasted.
func (s *RelaySplicer) charge(payloadLen int64) error {
	if s.Quota == nil {
		return nil
	}
	for {
		wait, err := s.Quota.Reserve(payloadLen)
		if err != nil {
			return err
		}
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			// Refilled (maybe — the flow's other direction may have
			// drunk from the same bucket, so the budget is asked again
			// rather than trusted).
		case <-s.Done:
			timer.Stop()
			return fmt.Errorf("%w: mesh: the flow ended while the splice waited for the rate budget",
				ErrRelaySpliceFlowEnded)
		}
	}
}

// copyBody copies n bytes of the record's body from br to to.
//
// A short read is a TRUNCATION, not a clean end: the prefix promised
// those bytes and they never came. The error does not wrap io.EOF, so
// the "EOF means the sender finished" callers cannot mistake it for a
// graceful finish.
func (s *RelaySplicer) copyBody(br *bufio.Reader, to io.Writer, n int64) error {
	for n > 0 {
		chunk := int64(len(s.scratch))
		if chunk > n {
			chunk = n
		}
		got, err := io.ReadFull(br, s.scratch[:int(chunk)])
		if err != nil {
			return fmt.Errorf("%w: mesh: %d of %d announced bytes arrived: %v",
				ErrRelaySpliceTruncated, got, n, err)
		}
		if err := writeFull(to, s.scratch[:got]); err != nil {
			return fmt.Errorf("mesh: relay: the spliced record could not be written: %w", err)
		}
		n -= int64(got)
	}
	return nil
}
