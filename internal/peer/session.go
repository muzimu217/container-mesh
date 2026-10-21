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
	"context"
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

	"github.com/muzimu217/container-mesh/internal/secure"
	"github.com/muzimu217/container-mesh/internal/wire"
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
	// MaxFileSize rejects inbound offers larger than this many bytes. Zero
	// means DefaultMaxFileSize; a negative value disables the check.
	MaxFileSize int64
	// AllowHidden permits receiving dotfiles. Off by default: the receiver
	// writes into a working directory, where a dropped .bashrc or .ssh entry
	// is a code-execution risk.
	AllowHidden bool
	// ForceOverwrite replaces an existing file with the same name instead of
	// refusing. Off by default so a peer cannot silently destroy local data.
	ForceOverwrite bool
}

// DefaultMaxFileSize caps inbound transfers at 16 GiB. The peer that offered a
// file is authenticated by the PAKE, but authentication is not a disk quota:
// without a ceiling one session can fill the receiver's volume.
const DefaultMaxFileSize int64 = 16 << 30

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
	// OnForwardListen receives a peer's request to host -R listeners and must
	// answer with one status per spec.
	//
	// It runs on the read pump goroutine, so it must not block: binding a
	// listener is fast, but dialling is not. A nil callback refuses every
	// request, which is the correct default for a peer that never enabled -R.
	OnForwardListen func(specs []wire.ForwardListenSpec) []wire.ForwardListenStatus
}

// Session is one peer's view of the conversation.
type Session struct {
	cfg Config
	// pump owns the encrypted channel: it is the only component that touches
	// secure.Session, and it also serves as the bootstrap FrameConn (transport
	// messages + AEAD).
	pump *Pump
	// frames is where frames come from and go to. In bootstrap mode it is the
	// pump itself; once StartMuxed has run it is the yamux control stream. The
	// session never learns which, which is what keeps the transfer code
	// untouched by the mux work.
	frames FrameConn
	log    *slog.Logger

	// PeerName is filled by the hello exchange.
	PeerName string
	// recvAtStart is the receive counter right after the hello exchange. The
	// hello itself is one frame, so the raw counter is never zero; what
	// matters for detecting a pairing that carried no data is the delta.
	recvAtStart uint64

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
	outboundCh chan *wire.Frame

	// Inbound direction: the transfer currently consuming chunk frames.
	inMu    sync.Mutex
	inbound *InboundTransfer

	// fwdMu serialises -R registration requests. Only one may be in flight: the
	// reply carries a result list but no request id, so two concurrent requests
	// could not tell their answers apart.
	fwdMu     sync.Mutex
	fwdResult chan []wire.ForwardListenStatus
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
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = DefaultMaxFileSize
	}
	// The pump is both the bootstrap FrameConn and the owner of the encrypted
	// channel; handing it the switchable wrapper is what keeps a p2p upgrade
	// working before the mux barrier.
	pump := NewPump(newSwitchable(conn), sec, PumpConfig{
		RecordSize: wire.MaxStreamData,
		BufBytes:   1 << 20,
		Logger:     cfg.Logger,
	})
	return &Session{
		cfg:        cfg,
		pump:       pump,
		frames:     pump,
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
	return s.StartWithNegotiation(h, nil)
}

// Negotiator runs after the hello exchange and before the read pump starts.
// It may replace the transport (via UpgradeToDirect) so that all subsequent
// traffic uses a direct path. Returning an error aborts the session; returning
// nil means "continue on whatever transport is current", which lets a failed
// hole-punch fall back to the relay transparently.
//
// It must not start goroutines that read the session: the read pump owns the
// channel from the moment this returns.
type Negotiator func(*Session) error

// StartWithNegotiation is Start with a direct-path negotiation step.
func (s *Session) StartWithNegotiation(h Handler, neg Negotiator) error {
	if err := s.exchangeHello(); err != nil {
		return err
	}
	s.handler = h
	if neg != nil {
		// The negotiator needs to read signalling frames itself, so the pump
		// is not running yet. Any frames it consumes beyond signalling are
		// buffered by the caller if needed; in practice the peers exchange
		// only p2p frames here.
		if err := neg(s); err != nil {
			return err
		}
	}
	s.recvAtStart = s.pump.RecvCounter()
	s.log.Info("session established", "peer", s.PeerName, "role", s.cfg.Role.String())
	go s.readPump()
	return nil
}

// MuxBoot performs the mux barrier, builds the yamux session, starts its accept
// loop, and returns the control stream. The CLI supplies it; the peer package
// stays free of a yamux dependency.
type MuxBoot func(*Session) (io.ReadWriteCloser, error)

// StartMuxed is StartWithNegotiation with a mux bootstrap step inserted between
// the negotiation and the read pump.
//
// The order is load-bearing:
//
//	hello -> negotiation (+ transport switch) -> mux barrier -> control stream -> read pump
//
// A8: the transport may only change before yamux exists, so negotiation comes
// first. The read pump may only start once the frame source is the control
// stream, or it would race StartMux for the next frame on the raw transport.
func (s *Session) StartMuxed(h Handler, neg Negotiator, boot MuxBoot) error {
	if err := s.exchangeHello(); err != nil {
		return err
	}
	s.handler = h
	if neg != nil {
		if err := neg(s); err != nil {
			return err
		}
	}
	if boot == nil {
		return errors.New("peer: StartMuxed needs a boot function")
	}
	ctl, err := boot(s)
	if err != nil {
		return err
	}
	if err := s.AttachControl(ctl); err != nil {
		return err
	}
	s.log.Info("session established (muxed)", "peer", s.PeerName, "role", s.cfg.Role.String())
	go s.readPump()
	return nil
}

// AttachControl installs a byte stream as the frame source.
//
// It must be called after the mux barrier and before the read pump starts.
// Once installed, frames travel as wire.Frame records on the control stream
// instead of as separate encrypted transport messages.
func (s *Session) AttachControl(rw io.ReadWriteCloser) error {
	if rw == nil {
		return errors.New("peer: nil control stream")
	}
	if !s.pump.Muxed() {
		return ErrPumpBootstrap
	}
	s.frames = newStreamFrameConn(rw)
	s.recvAtStart = s.pump.RecvCounter()
	return nil
}

// Pump exposes the record pump so the CLI can run the mux barrier.
func (s *Session) Pump() *Pump { return s.pump }

// readPump is the single reader. Everything inbound is dispatched here; no
// other goroutine may call RecvFrame.
func (s *Session) readPump() {
	var pumpErr error
	defer func() {
		s.errMu.Lock()
		s.pumpErr = pumpErr
		s.errMu.Unlock()
		// Unblock anyone waiting in waitOutbound/InboundTransfer.next.
		_ = s.frames.Close()
		close(s.done)
	}()

	for {
		f, err := s.recvFrame()
		if err != nil {
			if errors.Is(err, ErrTransportSwitched) {
				// A direct path replaced the relay transport. The session key
				// is unchanged (the switch happens inside the encrypted
				// channel), so simply continue reading on the new transport.
				//
				// This branch only applies in single-stream mode. Once the
				// record pump is muxed it refuses a mid-stream switch itself
				// (see Pump.readLoop), because a half-read record would be
				// lost and every yamux stream would die with it.
				s.log.Info("transport switched to direct path")
				continue
			}
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
		case wire.FrameForwardListen:
			s.handleForwardListen(f)
		case wire.FrameForwardListenResult:
			s.deliverForwardResult(f)
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
		if sendErr := s.SendFrame(&wire.Frame{Type: wire.FrameFileReject, Payload: []byte("bad offer payload")}); sendErr != nil {
			s.log.Warn("could not send offer rejection", "err", sendErr)
		}
		return
	}

	s.inMu.Lock()
	if s.inbound != nil {
		s.inMu.Unlock()
		if sendErr := s.SendFrame(&wire.Frame{Type: wire.FrameFileReject, Payload: []byte("busy: another inbound transfer is active")}); sendErr != nil {
			s.log.Warn("could not send busy rejection", "err", sendErr)
		}
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

// RequestForwardListen asks the peer to host the given listeners and waits for
// the answer.
//
// Only one request may be in flight at a time: the reply carries a result list
// but no request id, so two concurrent requests could not tell their answers
// apart. The mutex turns that into a queue rather than a silent mix-up.
func (s *Session) RequestForwardListen(ctx context.Context, specs []wire.ForwardListenSpec) ([]wire.ForwardListenStatus, error) {
	s.fwdMu.Lock()
	defer s.fwdMu.Unlock()

	ch := make(chan []wire.ForwardListenStatus, 1)
	s.errMu.Lock()
	s.fwdResult = ch
	s.errMu.Unlock()
	defer func() {
		s.errMu.Lock()
		s.fwdResult = nil
		s.errMu.Unlock()
	}()

	payload, err := json.Marshal(struct {
		Specs []wire.ForwardListenSpec `json:"specs"`
	}{Specs: specs})
	if err != nil {
		return nil, err
	}
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameForwardListen, Payload: payload}); err != nil {
		return nil, err
	}

	select {
	case res := <-ch:
		return res, nil
	case <-s.done:
		return nil, s.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deliverForwardResult hands a result list to the goroutine waiting in
// RequestForwardListen. A result with nobody waiting is a stray from an
// abandoned request and is dropped.
func (s *Session) deliverForwardResult(f *wire.Frame) {
	var res struct {
		Results []wire.ForwardListenStatus `json:"results"`
	}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		s.log.Warn("bad forward-listen result payload", "err", err)
		return
	}
	s.errMu.Lock()
	ch := s.fwdResult
	s.errMu.Unlock()
	if ch == nil {
		s.log.Debug("dropping unmatched forward-listen result")
		return
	}
	select {
	case ch <- res.Results:
	default:
	}
}

// handleForwardListen answers a peer's -R registration.
//
// A refusal is a normal answer, not an error: the peer asked for something this
// side does not permit, and the status says which. Returning an error here would
// tear down a session that is otherwise healthy.
func (s *Session) handleForwardListen(f *wire.Frame) {
	var req struct {
		Specs []wire.ForwardListenSpec `json:"specs"`
	}
	if err := json.Unmarshal(f.Payload, &req); err != nil {
		s.log.Warn("bad forward-listen payload", "err", err)
		s.replyForwardListen(nil)
		return
	}
	if s.handler.OnForwardListen == nil {
		out := make([]wire.ForwardListenStatus, 0, len(req.Specs))
		for _, sp := range req.Specs {
			out = append(out, wire.ForwardListenStatus{
				ID: sp.ID, Status: wire.StatusDenied,
				Message: "reverse forwarding is disabled on this peer",
			})
		}
		s.replyForwardListen(out)
		return
	}
	s.replyForwardListen(s.handler.OnForwardListen(req.Specs))
}

// replyForwardListen sends the answer, ignoring send failures: the peer may
// already be gone, and that is not worth failing the session over.
func (s *Session) replyForwardListen(results []wire.ForwardListenStatus) {
	payload, err := json.Marshal(struct {
		Results []wire.ForwardListenStatus `json:"results"`
	}{Results: results})
	if err != nil {
		s.log.Warn("could not encode forward-listen result", "err", err)
		return
	}
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameForwardListenResult, Payload: payload}); err != nil {
		s.log.Debug("could not send forward-listen result", "err", err)
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
// The pump holds the seal/send lock, so the counter order on the wire always
// matches the counter order the receiver expects.
func (s *Session) SendFrame(f *wire.Frame) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.frames.SendFrame(f)
}

// RecvFrame reads and opens one frame.
//
// It is exported for the direct-path negotiation, which runs between the hello
// exchange and the read pump. Calling it while the pump is running would
// compete for the same channel, so it must not be used afterwards.
func (s *Session) RecvFrame() (*wire.Frame, error) { return s.recvFrame() }

// recvFrame reads and opens one frame. Only the read pump (and the phases
// before it starts: hello and p2p negotiation) may call this.
func (s *Session) recvFrame() (*wire.Frame, error) {
	return s.frames.RecvFrame()
}

// SendMessage sends a small text message.
func (s *Session) SendMessage(text string) error {
	return s.SendFrame(&wire.Frame{Type: wire.FrameMsg, Payload: []byte(text)})
}

// ErrPathLocked is returned by UpgradeToDirect once the record pump has passed
// the mux barrier.
//
// A switch after that point is not a small risk: the pump cuts the byte stream
// into records, and a switch mid-record discards the bytes already read from
// the old transport. The half-record is unrecoverable, so the next OpenFrame
// is misaligned and every multiplexed stream dies at once. Refusing loudly is
// better than pretending to migrate losslessly.
var ErrPathLocked = errors.New("peer: transport path is locked for the life of a muxed session")

// UpgradeToDirect replaces the relay transport with a direct one.
//
// It is safe to call while the read pump is blocked in Recv: the old socket is
// closed after the new transport is installed, and the pump recognises the
// resulting error as a switch rather than a failure.
//
// The caller must have completed the p2p handshake on the *encrypted* channel,
// so the session key and frame counters continue unchanged.
//
// Once the pump is muxed this returns ErrPathLocked: see the type comment on
// ErrPathLocked for why a hot switch is not attempted.
func (s *Session) UpgradeToDirect(next MessageConn) error {
	if next == nil {
		return errors.New("peer: nil transport")
	}
	if s.pump.Muxed() {
		return ErrPathLocked
	}
	sw, ok := s.pump.conn.(*switchable)
	if !ok {
		return errors.New("peer: transport is not switchable")
	}
	return sw.Switch(next)
}

// OnDirect reports whether the session is currently on a direct path.
func (s *Session) OnDirect() bool {
	sw, ok := s.pump.conn.(*switchable)
	return ok && sw.Switched()
}

// ShortAuth returns the 6-digit short authentication string both peers derive
// from the session key. Users compare it out of band; a mismatch means a third
// party sat in the middle (the relay knows the room label, so it can route but
// must not be trusted to have paired the intended peers).
func (s *Session) ShortAuth() string { return s.pump.ShortAuth() }

// Fingerprint is a longer digest of the same key, for logs and audit trails.
func (s *Session) Fingerprint() string { return s.pump.Fingerprint() }

// FramesSinceStart reports how many frames arrived after the session was
// established (the hello exchange itself is excluded).
//
// A session that ends with zero such frames never really carried anything: the
// relay pairs on slot presence, so a peer that died while parked can be paired
// with a live one, and the splice then fails on its first read. Callers use
// this to retry immediately instead of treating it as a normal disconnect.
func (s *Session) FramesSinceStart() uint64 {
	now := s.pump.RecvCounter()
	if now <= s.recvAtStart {
		return 0
	}
	return now - s.recvAtStart
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

	// A directory is packed into a temporary tar and sent as one ordinary
	// file. Packing first (rather than streaming) costs disk space but keeps
	// the whole existing path — offer, resume, hash verification — unchanged,
	// and a resumed transfer of a directory then works exactly like a file.
	sendPath := path
	sendName := filepath.Base(path)
	if fi.IsDir() {
		archive, packErr := packDirToTemp(path)
		if packErr != nil {
			return nil, packErr
		}
		defer os.Remove(archive)
		sendPath = archive
		sendName = ArchiveName(path)
		if fi, err = os.Stat(archive); err != nil {
			return nil, err
		}
	}

	sum, err := HashFile(sendPath)
	if err != nil {
		return nil, err
	}
	offer := wire.FileOffer{
		Name:   sendName,
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

	fh, err := os.Open(sendPath)
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
	if err := validateNamePolicy(offer.Name, s.cfg.AllowHidden); err != nil {
		tr.Reject(err.Error())
		return &wire.FileResult{OK: false, Message: err.Error()}, nil
	}
	if offer.Size < 0 {
		tr.Reject("negative size")
		return &wire.FileResult{OK: false, Message: "negative size"}, nil
	}
	// A peer that completed the PAKE is authenticated, but that says nothing
	// about how much disk it may consume: without a ceiling one session can
	// fill the volume. The limit is configurable, not a protocol constant.
	if s.cfg.MaxFileSize > 0 && offer.Size > s.cfg.MaxFileSize {
		msg := fmt.Sprintf("file too large: %d bytes exceeds limit %d", offer.Size, s.cfg.MaxFileSize)
		tr.Reject(msg)
		return &wire.FileResult{OK: false, Message: msg}, nil
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
	// have is how many bytes we already hold; havePath is where they live.
	// They must be tracked separately: for a zero-byte file have == 0 ==
	// offer.Size from the start, and treating that as "already complete" sent
	// the code down a path that opened an empty file name (ENOENT), so empty
	// files could never be received.
	var (
		have     int64
		havePath string
	)
	if fi, err := os.Stat(partPath); err == nil {
		have = fi.Size()
		havePath = partPath
		if have > offer.Size {
			// Longer than the target: definitely stale.
			have = 0
			havePath = ""
			if rmErr := os.Remove(partPath); rmErr != nil && !os.IsNotExist(rmErr) {
				s.log.Warn("could not remove stale part file", "path", partPath, "err", rmErr)
			}
		}
	} else if fi, err := os.Stat(finalPath); err == nil && fi.Size() == offer.Size {
		// The complete file is already here. Ask the sender for nothing and
		// verify immediately.
		have = offer.Size
		havePath = finalPath
	}

	// Refuse before writing when the volume cannot hold what is still missing.
	// Without this the transfer fails at some arbitrary point, possibly after
	// filling the disk for every other process on the host.
	if need := offer.Size - have; need > 0 {
		if free, err := freeDiskBytes(dir); err == nil && free < need {
			msg := fmt.Sprintf("insufficient disk space: need %d bytes, %d available", need, free)
			tr.Reject(msg)
			return &wire.FileResult{OK: false, Message: msg}, nil
		}
	}

	acc, _ := json.Marshal(wire.FileAccept{ResumeFrom: have})
	if err := s.SendFrame(&wire.Frame{Type: wire.FrameFileAccept, Payload: acc}); err != nil {
		return nil, err
	}

	// "Already complete" means the bytes are in the FINAL file. A zero-byte
	// .part also has have == offer.Size == 0, but it still has to go through
	// the normal publish path (sync/close/hash/rename) to become the final
	// file.
	alreadyFinal := havePath == finalPath && have == offer.Size

	var out *os.File
	if alreadyFinal {
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
		// Record where the bytes will live. Without this a zero-byte transfer
		// (where the loop below never runs) would look for its data in the
		// final path, which does not exist yet.
		havePath = partPath
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
		if rmErr := os.Remove(partPath); rmErr != nil && !os.IsNotExist(rmErr) {
			s.log.Warn("could not remove part file after hash mismatch", "path", partPath, "err", rmErr)
		}
		res := wire.FileResult{OK: false, Message: "sender hash differs from offer"}
		rb, _ := json.Marshal(res)
		if sendErr := s.SendFrame(&wire.Frame{Type: wire.FrameFileResult, Payload: rb}); sendErr != nil {
			s.log.Warn("could not report hash mismatch to sender", "err", sendErr)
		}
		return &res, nil
	}

	// Verify. The source is chosen by where the bytes live, not by the byte
	// count: a zero-byte transfer also has have == offer.Size, but its data is
	// in the .part file that still needs sync/close/rename.
	fromPart := !alreadyFinal
	var sum string
	if !fromPart {
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
		if fromPart {
			// Rename must not clobber an unrelated file that happens to share
			// the name. Link-then-remove gives atomic "create if absent"
			// semantics; on filesystems without hard links, fall back to an
			// explicit existence check.
			if pubErr := publishNoClobber(partPath, finalPath, s.cfg.ForceOverwrite); pubErr != nil {
				// The sender is waiting for a verdict: reporting this as a
				// local error would leave it blocked until its timeout. Turn
				// it into a rejection it can act on.
				res = wire.FileResult{OK: false, Message: pubErr.Error()}
			} else if IsArchiveName(offer.Name) {
				// A packed directory: extract it next to the archive and
				// remove the archive, so the receiver sees the tree it was
				// sent rather than an opaque tar.
				if upErr := UnpackArchive(finalPath, dir, s.cfg.ForceOverwrite); upErr != nil {
					res = wire.FileResult{OK: false, Message: "unpack failed: " + upErr.Error()}
				} else if rmErr := os.Remove(finalPath); rmErr != nil {
					s.log.Warn("could not remove archive after unpack", "path", finalPath, "err", rmErr)
				}
			}
		}
	} else {
		res.Message = fmt.Sprintf("hash mismatch: got %s, want %s", sum, offer.SHA256)
		// A mismatching partial file is useless; drop it so the next attempt
		// is clean.
		if fromPart {
			if rmErr := os.Remove(partPath); rmErr != nil && !os.IsNotExist(rmErr) {
				s.log.Warn("could not remove failed part file", "path", partPath, "err", rmErr)
			}
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
		// Close the pump first: it owns the transport and it also unblocks the
		// record queues that a muxed session's yamux is parked on.
		s.closeErr = s.pump.Close()
	})
	return s.closeErr
}

// maxNameLen bounds the received file name. 255 bytes is the limit most
// filesystems impose on a single path component.
const maxNameLen = 255

// reservedNames are rejected regardless of case: they are device names on
// Windows, and a receiver on a shared volume should not create them.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// validateName rejects path traversal and other nonsense file names.
//
// The allow-list is deliberately narrow: letters, digits, dot, dash and
// underscore. Anything else (control characters, colons, quotes, shell
// metacharacters, unicode look-alikes) is refused rather than escaped, because
// the receiver writes the name straight to disk.
func validateName(name string) error { return validateNamePolicy(name, false) }

// validateNamePolicy is validateName with the hidden-file policy as a
// parameter, so the default stays strict and the opt-in is explicit.
func validateNamePolicy(name string, allowHidden bool) error {
	if name == "" {
		return errors.New("empty file name")
	}
	if name == "." || name == ".." {
		return errors.New("invalid file name")
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("file name must not contain path separators")
	}
	if strings.HasPrefix(name, ".") && !allowHidden {
		return errors.New("refusing to receive hidden file (use --allow-hidden to accept)")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("file name too long (%d bytes, max %d)", len(name), maxNameLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '+', r == ' ',
			r == '(', r == ')', r == '[', r == ']', r == '@', r == '#':
			// Common in real file names (downloads, screenshots, exports).
			// None of these are shell metacharacters when the name is passed
			// as a single argv element, and none are path separators.
		default:
			return fmt.Errorf("file name contains unsupported character %q", r)
		}
	}
	if strings.HasPrefix(name, " ") || strings.HasSuffix(name, " ") {
		return errors.New("file name must not start or end with a space")
	}
	// ".part" is the receiver's own in-progress suffix; accepting it would let
	// a peer collide with another transfer's temporary file.
	if strings.HasSuffix(strings.ToLower(name), ".part") {
		return errors.New("refusing reserved suffix .part")
	}
	// Windows device names, with or without an extension ("CON", "con.txt").
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if reservedNames[strings.ToLower(base)] {
		return fmt.Errorf("refusing reserved device name %q", name)
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
