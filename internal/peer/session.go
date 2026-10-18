// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Package peer implements the application session on top of an established
// encrypted channel: hello exchange, small messages, and resumable file
// transfer in both directions at once.
//
// Architecture: exactly one goroutine (the "read pump", started by Start)
// ever reads from the transport. It dispatches frames by type:
//
//   - FramePing is answered automatically, so transfers never have to worry
//     about keepalive interleaving with their data.
//   - FrameMsg / FrameError go to the Handler callbacks.
//   - FrameFileChunk / FrameFileDone go to the active InboundTransfer.
//   - FrameFileAccept / FrameFileReject / FrameFileResult go to whichever
//     goroutine is inside SendFile, via a dedicated channel.
//
// This routing is what makes the two transfer directions independent: a
// sender only writes chunks and waits for accept/result frames (which belong
// to its own offer), while a receiver consumes chunk frames. Neither ever
// races the other for the socket.
package peer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blackevil/container-mesh/internal/secure"
	"github.com/blackevil/container-mesh/internal/wire"
)

// ChunkSize is the file payload per frame. 512 KiB keeps a frame inside the
// 1 MiB wire limit while cutting per-frame overhead (AEAD, WebSocket header,
// relay splice) enough to matter on real wide-area links, where throughput
// is dominated by per-message cost rather than bandwidth.
const ChunkSize = 512 << 10

// MessageConn is the minimal message transport the session needs. The
// production implementation is *wsconn.Conn; tests use an in-memory pipe.
type MessageConn interface {
	// Send writes one complete message.
	Send([]byte) error
	// Recv reads one complete message.
	Recv() ([]byte, error)
	// Close releases the transport.
	Close() error
}

// Config configures a peer session.
type Config struct {
	// Name is advertised in the hello exchange.
	Name string
	// Code is the shared transfer code.
	Code string
	// Role is A (initiator) or B (responder).
	Role secure.Role
	// Logger receives structured events.
	Logger *slog.Logger
}

// Handler receives events from the session's read pump. Callbacks run on the
// pump goroutine; they must be cheap unless documented otherwise.
type Handler struct {
	// OnMessage receives an inbound chat message.
	OnMessage func(text string)
	// OnError receives a protocol error reported by the peer.
	OnError func(text string)
	// OnOffer is called when the peer offers a file. Return true to take
	// ownership: you must then call Accept or Reject on the transfer, from
	// this goroutine or another (Accept blocks on chunk delivery, so real
	// callers will run it in a goroutine and return immediately). Returning
	// false rejects the offer right away. A nil OnOffer rejects every offer.
	OnOffer func(tr *InboundTransfer) bool
}

// Session is one peer's view of the conversation.
type Session struct {
	cfg  Config
	conn MessageConn
	sec  *secure.Session
	log  *slog.Logger

	// PeerName is filled by the hello exchange.
	PeerName string

	sendMu sync.Mutex

	closeOnce sync.Once
	closeErr  error

	// done is closed when the read pump exits; errMu guards pumpErr.
	done    chan struct{}
	errMu   sync.Mutex
	pumpErr error

	handler Handler

	// Outbound direction: one SendFile at a time; responses are routed here.
	outMu      sync.Mutex
	outbound   bool
	outboundCh chan *wire.Frame

	// Inbound direction: the transfer currently consuming chunk frames.
	inMu    sync.Mutex
	inbound *InboundTransfer
}

// ErrHandshake is returned when the peers cannot agree on a key, which almost
// always means the codes differ. It is permanent: retrying with the same code
// and the same peer cannot succeed.
var ErrHandshake = errors.New("peer: handshake failed (check that both sides use the same code)")

// ErrProtocol covers permanent protocol disagreements (version, role clash).
// Unlike transport errors these are not worth reconnecting over.
var ErrProtocol = errors.New("peer: protocol error")

// Handshake performs the SPAKE2 exchange over conn.
//
// The exchange is: A sends msg1, B replies with msg2. Both sides then derive
// the same key. A's first frame is sent immediately; B blocks waiting for it.
func Handshake(conn MessageConn, cfg Config) (*Session, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	h, err := secure.NewHandshake(cfg.Code, cfg.Role)
	if err != nil {
		// Bad code length/format is a deterministic configuration error, not
		// a transient network condition: reconnecting cannot fix it.
		return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
	}

	var out []byte
	if cfg.Role == secure.RoleA {
		out, _, err = h.Step(nil)
		if err != nil {
			return nil, err
		}
		if err := conn.Send(out); err != nil {
			return nil, err
		}
		// Wait for B's reply.
		in, err := conn.Recv()
		if err != nil {
			return nil, fmt.Errorf("%w: reading peer step: %v", ErrHandshake, err)
		}
		if _, done, err := h.Step(in); err != nil || !done {
			if err == nil {
				err = errors.New("handshake did not complete")
			}
			return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
	} else {
		in, err := conn.Recv()
		if err != nil {
			return nil, fmt.Errorf("%w: reading peer step: %v", ErrHandshake, err)
		}
		out, _, err = h.Step(in)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
		if err := conn.Send(out); err != nil {
			return nil, err
		}
	}

	sec, err := secure.NewSession(h)
	if err != nil {
		return nil, err
	}
	return &Session{
		cfg:        cfg,
		conn:       conn,
		sec:        sec,
		log:        cfg.Logger,
		done:       make(chan struct{}),
		outboundCh: make(chan *wire.Frame, 8),
	}, nil
}

// exchangeHello sends our hello and validates the peer's.
//
// The exchange is symmetric: each side sends exactly one FrameHello and reads
// exactly one. No acknowledgement is needed: the encrypted channel already
// proves both sides hold the same key, so the hello only carries presentation
// data (name, version, role).
func (s *Session) exchangeHello() error {
	hello := map[string]any{
		"version": wire.Version,
		"name":    s.cfg.Name,
		"role":    s.cfg.Role.String(),
	}
	payload, _ := json.Marshal(hello)
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameHello, Payload: payload}); err != nil {
		return err
	}

	f, err := s.recvFrame()
	if err != nil {
		return err
	}
	if f.Type != wire.FrameHello {
		return fmt.Errorf("%w: expected hello, got %s", ErrProtocol, f.Type)
	}
	var theirs struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
		Role    string `json:"role"`
	}
	if err := json.Unmarshal(f.Payload, &theirs); err != nil {
		return fmt.Errorf("%w: bad hello: %v", ErrProtocol, err)
	}
	if theirs.Version != wire.Version {
		return fmt.Errorf("%w: protocol version mismatch: peer %d, ours %d", ErrProtocol, theirs.Version, wire.Version)
	}
	if theirs.Role == s.cfg.Role.String() {
		return fmt.Errorf("%w: both sides claim role %s", ErrProtocol, theirs.Role)
	}
	s.PeerName = theirs.Name
	return nil
}

// Start runs the hello exchange and then starts the read pump. Call it once,
// immediately after Handshake. All events are delivered via h.
func (s *Session) Start(h Handler) error {
	if err := s.exchangeHello(); err != nil {
		return err
	}
	s.handler = h
	s.log.Info("session established", "peer", s.PeerName, "role", s.cfg.Role.String())
	go s.readPump()
	return nil
}

// readPump is the single reader. Everything inbound is dispatched here; no
// other goroutine may call Recv.
func (s *Session) readPump() {
	var pumpErr error
	defer func() {
		s.errMu.Lock()
		s.pumpErr = pumpErr
		s.errMu.Unlock()
		// Unblock anyone waiting in waitOutbound/InboundTransfer.next.
		_ = s.conn.Close()
		close(s.done)
	}()

	for {
		f, err := s.recvFrame()
		if err != nil {
			pumpErr = err
			return
		}
		switch f.Type {
		case wire.FramePing:
			_ = s.SendFrame(&wire.Frame{Type: wire.FramePong, Payload: f.Payload})
		case wire.FramePong:
			// Nothing to do; transports answer their own keepalive.
		case wire.FrameMsg:
			if h := s.handler.OnMessage; h != nil {
				h(string(f.Payload))
			}
		case wire.FrameError:
			if h := s.handler.OnError; h != nil {
				h(string(f.Payload))
			}
		case wire.FrameHello:
			s.log.Debug("unexpected duplicate hello ignored")
		case wire.FrameFileOffer:
			s.handleOffer(f)
		case wire.FrameFileChunk, wire.FrameFileDone:
			if !s.deliverInbound(f) {
				pumpErr = fmt.Errorf("peer: received %s with no active inbound transfer", f.Type)
				return
			}
		case wire.FrameFileAccept, wire.FrameFileReject, wire.FrameFileResult:
			s.deliverOutbound(f)
		default:
			// Ignore unknown frames so the protocol can grow.
		}
	}
}

// handleOffer routes an inbound offer to the handler, enforcing a single
// active inbound transfer per session.
func (s *Session) handleOffer(f *wire.Frame) {
	var offer wire.FileOffer
	if err := json.Unmarshal(f.Payload, &offer); err != nil {
		_ = s.SendFrame(&wire.Frame{Type: wire.FrameFileReject, Payload: []byte("bad offer payload")})
		return
	}

	s.inMu.Lock()
	if s.inbound != nil {
		s.inMu.Unlock()
		_ = s.SendFrame(&wire.Frame{Type: wire.FrameFileReject, Payload: []byte("busy: another inbound transfer is active")})
		return
	}
	tr := &InboundTransfer{
		Offer:   offer,
		session: s,
		frames:  make(chan *wire.Frame, 8),
	}
	s.inbound = tr
	s.inMu.Unlock()

	taken := false
	if s.handler.OnOffer != nil {
		taken = s.handler.OnOffer(tr)
	}
	if !taken {
		tr.Reject("offer not accepted")
	}
}

// deliverInbound hands a chunk frame to the active inbound transfer. It
// reports whether one existed. Blocking here is deliberate backpressure: the
// receiver's disk is the slowest link in the chain.
func (s *Session) deliverInbound(f *wire.Frame) bool {
	s.inMu.Lock()
	tr := s.inbound
	s.inMu.Unlock()
	if tr == nil {
		return false
	}
	tr.frames <- f
	return true
}

// deliverOutbound hands an accept/reject/result to whoever offered a file.
// Responses are small and expected promptly; a full channel means the
// response is stale, so dropping beats blocking the pump.
func (s *Session) deliverOutbound(f *wire.Frame) {
	select {
	case s.outboundCh <- f:
	default:
		s.log.Warn("dropping stale transfer response", "type", f.Type.String())
	}
}

func (s *Session) clearInbound(tr *InboundTransfer) {
	s.inMu.Lock()
	if s.inbound == tr {
		s.inbound = nil
	}
	s.inMu.Unlock()
}

// SendFrame seals and sends one frame. It is safe for concurrent use.
//
// The seal and the send must happen under the same lock. The receiver rejects
// any frame whose counter is not exactly the next expected value, so if two
// goroutines sealed concurrently and then raced to the socket, the higher
// counter could hit the wire first and the session would fail with a decrypt
// error.
func (s *Session) SendFrame(f *wire.Frame) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	b, err := s.sec.SealFrame(f)
	if err != nil {
		return err
	}
	return s.conn.Send(b)
}

// recvFrame reads and opens one frame. Only the read pump (and the hello
// exchange, which runs before the pump starts) may call this.
func (s *Session) recvFrame() (*wire.Frame, error) {
	b, err := s.conn.Recv()
	if err != nil {
		return nil, err
	}
	return s.sec.OpenFrame(b)
}

// SendMessage sends a small text message.
func (s *Session) SendMessage(text string) error {
	return s.SendFrame(&wire.Frame{Type: wire.FrameMsg, Payload: []byte(text)})
}

// SendFile offers and streams one file, waiting for the receiver to verify it.
//
// Resume works like this:
//  1. The sender offers name+size+sha256.
//  2. The receiver replies with the number of bytes it already has on disk
//     (its partial file length; a stale or corrupt prefix cannot survive
//     because the whole-file hash is verified before the file is renamed
//     into place).
//  3. The sender seeks to that offset and streams from there.
//
// While streaming, this goroutine only writes; the read pump keeps handling
// pings, messages and the reverse direction's traffic concurrently.
func (s *Session) SendFile(path string, progress func(sent, total int64)) (*wire.FileResult, error) {
	s.outMu.Lock()
	defer s.outMu.Unlock()

	// Drop responses left over from an interrupted previous transfer so a
	// stale accept/result cannot be mistaken for ours.
drain:
	for {
		select {
		case <-s.outboundCh:
		default:
			break drain
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("peer: stat %s: %w", path, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("peer: %s is a directory (only single files are supported)", path)
	}

	sum, err := HashFile(path)
	if err != nil {
		return nil, err
	}
	offer := wire.FileOffer{
		Name:   filepath.Base(path),
		Size:   fi.Size(),
		SHA256: sum,
	}
	ob, _ := json.Marshal(offer)
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameFileOffer, Payload: ob}); err != nil {
		return nil, err
	}

	// Wait for accept or reject.
	f, err := s.waitOutbound(offerWaitTimeout)
	if err != nil {
		return nil, err
	}
	switch f.Type {
	case wire.FrameFileReject:
		return &wire.FileResult{OK: false, Message: string(f.Payload)}, nil
	case wire.FrameFileAccept:
	default:
		return nil, fmt.Errorf("peer: expected accept/reject, got %s", f.Type)
	}
	var acc wire.FileAccept
	if err := json.Unmarshal(f.Payload, &acc); err != nil {
		return nil, fmt.Errorf("peer: bad accept payload: %w", err)
	}
	if acc.ResumeFrom < 0 || acc.ResumeFrom > fi.Size() {
		return nil, fmt.Errorf("peer: nonsensical resume offset %d for %d-byte file", acc.ResumeFrom, fi.Size())
	}

	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	if acc.ResumeFrom > 0 {
		s.log.Info("resuming transfer", "file", offer.Name, "from", acc.ResumeFrom, "total", fi.Size())
		if _, err := fh.Seek(acc.ResumeFrom, io.SeekStart); err != nil {
			return nil, err
		}
	}

	buf := make([]byte, ChunkSize)
	offset := acc.ResumeFrom
	if progress != nil {
		// Report the starting point before the first chunk so callers can
		// observe where the transfer resumed from.
		progress(offset, fi.Size())
	}
	for {
		n, rerr := io.ReadFull(fh, buf)
		if n > 0 {
			payload := wire.EncodeChunk(offset, buf[:n])
			if err := s.SendFrame(&wire.Frame{Type: wire.FrameFileChunk, Payload: payload}); err != nil {
				return nil, err
			}
			offset += int64(n)
			if progress != nil {
				progress(offset, fi.Size())
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("peer: read %s: %w", path, rerr)
		}
	}

	// Announce completion with the authoritative hash.
	raw, err := hexDecode(sum)
	if err != nil {
		return nil, err
	}
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameFileDone, Payload: raw}); err != nil {
		return nil, err
	}

	// Wait for the receiver's verdict.
	f, err = s.waitOutbound(resultWaitTimeout)
	if err != nil {
		return nil, err
	}
	if f.Type != wire.FrameFileResult {
		return nil, fmt.Errorf("peer: expected file result, got %s", f.Type)
	}
	var res wire.FileResult
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		return nil, fmt.Errorf("peer: bad result payload: %w", err)
	}
	return &res, nil
}

// Timeouts bounding the two points where SendFile waits on the peer. The
// offer timeout catches "nobody is there to receive" so a one-shot sender
// terminates; the result timeout is generous because it only starts after
// the last byte was handed to the transport.
const (
	offerWaitTimeout  = 30 * time.Second
	resultWaitTimeout = 10 * time.Minute
)

// waitOutbound blocks until the peer answers our offer or the session dies.
// A deadline turns "the receiver never showed up" into an error instead of
// an infinite wait.
func (s *Session) waitOutbound(timeout time.Duration) (*wire.Frame, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case f := <-s.outboundCh:
		return f, nil
	case <-s.done:
		return nil, s.Err()
	case <-t.C:
		return nil, fmt.Errorf("peer: no response within %s", timeout)
	}
}

// InboundTransfer is a file the peer has offered. Obtain one from
// Handler.OnOffer, then either Accept it into a directory or Reject it.
type InboundTransfer struct {
	Offer wire.FileOffer

	session *Session
	frames  chan *wire.Frame
}

// Accept receives the offered file into dir, verifying the hash before the
// file is renamed into place.
//
// The on-disk file is written as <name>.part and renamed on success, so an
// interrupted transfer leaves a resumable artifact and never a corrupt
// "complete" file. Accept may be called from any goroutine; it blocks until
// the transfer finishes or the session dies.
func (tr *InboundTransfer) Accept(dir string, progress func(got, total int64)) (*wire.FileResult, error) {
	defer tr.session.clearInbound(tr)
	return tr.accept(dir, progress)
}

// Reject declines the offer.
func (tr *InboundTransfer) Reject(reason string) {
	tr.session.clearInbound(tr)
	_ = tr.session.SendFrame(&wire.Frame{Type: wire.FrameFileReject, Payload: []byte(reason)})
}

// next waits for the next transfer frame from the read pump.
func (tr *InboundTransfer) next() (*wire.Frame, error) {
	select {
	case f := <-tr.frames:
		return f, nil
	case <-tr.session.done:
		return nil, tr.session.Err()
	}
}

func (tr *InboundTransfer) accept(dir string, progress func(got, total int64)) (*wire.FileResult, error) {
	s := tr.session
	offer := tr.Offer
	if err := validateName(offer.Name); err != nil {
		tr.Reject(err.Error())
		return &wire.FileResult{OK: false, Message: err.Error()}, nil
	}
	if offer.Size < 0 {
		tr.Reject("negative size")
		return &wire.FileResult{OK: false, Message: "negative size"}, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("peer: mkdir %s: %w", dir, err)
	}
	finalPath := filepath.Join(dir, offer.Name)
	partPath := finalPath + ".part"

	// Work out how much of the file we already have. A partial file is only
	// reusable if its length fits; its contents are proven when the whole
	// file hash is checked at the end. If that fails, the .part is deleted
	// and the next attempt starts from zero, so a bad prefix cannot survive
	// more than one retry.
	var have int64
	if fi, err := os.Stat(partPath); err == nil {
		have = fi.Size()
		if have > offer.Size {
			// Longer than the target: definitely stale.
			have = 0
			_ = os.Remove(partPath)
		}
	} else if fi, err := os.Stat(finalPath); err == nil && fi.Size() == offer.Size {
		// The complete file is already here. Ask the sender for nothing and
		// verify immediately.
		have = offer.Size
	}

	acc, _ := json.Marshal(wire.FileAccept{ResumeFrom: have})
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameFileAccept, Payload: acc}); err != nil {
		return nil, err
	}

	var out *os.File
	if have == offer.Size {
		// Already complete: open read-only and just verify.
		fh, openErr := os.Open(finalPath)
		if openErr != nil {
			return nil, openErr
		}
		out = fh
		defer out.Close()
	} else {
		fh, openErr := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if openErr != nil {
			return nil, fmt.Errorf("peer: open %s: %w", partPath, openErr)
		}
		out = fh
		defer out.Close()
		if _, seekErr := out.Seek(have, io.SeekStart); seekErr != nil {
			return nil, seekErr
		}
	}

	written := have
	if progress != nil {
		progress(written, offer.Size)
	}

	for written < offer.Size {
		f, err := tr.next()
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case wire.FrameFileChunk:
			off, data, err := wire.DecodeChunk(f.Payload)
			if err != nil {
				return nil, err
			}
			if off != written {
				return nil, fmt.Errorf("peer: out-of-order chunk: got offset %d, want %d", off, written)
			}
			if written+int64(len(data)) > offer.Size {
				return nil, fmt.Errorf("peer: chunk overruns declared size")
			}
			if _, err := out.Write(data); err != nil {
				return nil, fmt.Errorf("peer: write: %w", err)
			}
			written += int64(len(data))
			if progress != nil {
				progress(written, offer.Size)
			}
		case wire.FrameFileDone:
			return nil, fmt.Errorf("peer: sender finished at %d of %d bytes", written, offer.Size)
		default:
			return nil, fmt.Errorf("peer: unexpected frame %s during transfer", f.Type)
		}
	}

	// The sender always terminates a transfer with FrameFileDone, even when it
	// had nothing left to send after a resume. Consume it so the stream stays
	// in sync for the next operation.
	done, err := tr.next()
	if err != nil {
		return nil, err
	}
	if done.Type != wire.FrameFileDone {
		return nil, fmt.Errorf("peer: expected file-done, got %s", done.Type)
	}
	if len(done.Payload) != 32 {
		return nil, fmt.Errorf("peer: file-done payload must be 32 bytes, got %d", len(done.Payload))
	}
	sentHash := hexEncode(done.Payload)
	if sentHash != offer.SHA256 {
		// The sender hashed a different file than the one it offered. Do not
		// keep anything.
		_ = os.Remove(partPath)
		res := wire.FileResult{OK: false, Message: "sender hash differs from offer"}
		rb, _ := json.Marshal(res)
		_ = s.SendFrame(&wire.Frame{Type: wire.FrameFileResult, Payload: rb})
		return &res, nil
	}

	// Verify.
	var sum string
	if have == offer.Size {
		sum, err = HashFile(finalPath)
	} else {
		if err := out.Sync(); err != nil {
			return nil, err
		}
		if err := out.Close(); err != nil {
			return nil, err
		}
		sum, err = HashFile(partPath)
	}
	if err != nil {
		return nil, err
	}

	res := wire.FileResult{OK: sum == offer.SHA256}
	if res.OK {
		res.Message = "hash verified: " + sum
		if have != offer.Size {
			if err := os.Rename(partPath, finalPath); err != nil {
				return nil, fmt.Errorf("peer: rename: %w", err)
			}
		}
	} else {
		res.Message = fmt.Sprintf("hash mismatch: got %s, want %s", sum, offer.SHA256)
		// A mismatching partial file is useless; drop it so the next attempt
		// is clean.
		if have != offer.Size {
			_ = os.Remove(partPath)
		}
	}

	rb, _ := json.Marshal(res)
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameFileResult, Payload: rb}); err != nil {
		return nil, err
	}
	return &res, nil
}

// Done reports a channel that is closed once the session is finished (the
// transport failed or Close was called).
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns the error that ended the session, or nil if it is still alive.
func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.pumpErr
}

// Close shuts the session down. It is safe to call more than once and safe to
// call while another goroutine waits on Done.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}

// validateName rejects path traversal and other nonsense file names.
func validateName(name string) error {
	if name == "" {
		return errors.New("empty file name")
	}
	if name == "." || name == ".." {
		return errors.New("invalid file name")
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("file name must not contain path separators")
	}
	if strings.HasPrefix(name, ".") {
		return errors.New("refusing to receive hidden file")
	}
	return nil
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("peer: odd-length hex string")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, err := hexVal(s[2*i])
		if err != nil {
			return nil, err
		}
		lo, err := hexVal(s[2*i+1])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("peer: invalid hex digit %q", c)
	}
}
