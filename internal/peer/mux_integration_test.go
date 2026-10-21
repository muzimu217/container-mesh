// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package peer_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/mux"
	"github.com/muzimu217/container-mesh/internal/peer"
	"github.com/muzimu217/container-mesh/internal/secure"
)

// TestFileTransferOverMux is step 1.5 of the PR #1 plan: the complete existing
// file-transfer path must work with peer.Session running on a yamux control
// stream instead of on raw encrypted transport messages.
//
// This is the integration proof that the record pump, the barrier, the control
// stream and the untouched transfer code compose. If any of them is wrong, the
// hash at the end differs or the transfer never completes.
func TestFileTransferOverMux(t *testing.T) {
	const size = 4 << 20 // 4 MiB: big enough to span many 32 KiB records

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "payload.bin")
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(payload)

	ca, cb := newMsgConnPair()

	type res struct {
		sess *peer.Session
		err  error
	}
	chA := make(chan res, 1)
	chB := make(chan res, 1)

	// Role A: initiator. It crosses the barrier and opens the control stream.
	go func() {
		sess, err := peer.Handshake(ca, peer.Config{
			Name: "alpha", Code: "mux-file-code", Role: secure.RoleA,
		})
		if err != nil {
			chA <- res{nil, err}
			return
		}
		err = sess.StartMuxed(peer.Handler{}, nil, func(s *peer.Session) (io.ReadWriteCloser, error) {
			link, err := s.Pump().StartMux(context.Background())
			if err != nil {
				return nil, err
			}
			ms, err := mux.New(link, mux.DefaultConfig(mux.RoleClient))
			if err != nil {
				return nil, err
			}
			go serveControl(ms)
			return ms.Open(context.Background(), mux.KindControl)
		})
		chA <- res{sess, err}
	}()

	// Role B: responder. It crosses the barrier and accepts the control stream.
	go func() {
		sess, err := peer.Handshake(cb, peer.Config{
			Name: "beta", Code: "mux-file-code", Role: secure.RoleB,
			MaxFileSize: 0,
		})
		if err != nil {
			chB <- res{nil, err}
			return
		}
		ctlCh := make(chan io.ReadWriteCloser, 1)
		err = sess.StartMuxed(peer.Handler{
			OnOffer: func(tr *peer.InboundTransfer) bool {
				go func() {
					if _, err := tr.Accept(dstDir, nil); err != nil {
						t.Logf("accept: %v", err)
					}
				}()
				return true
			},
		}, nil, func(s *peer.Session) (io.ReadWriteCloser, error) {
			link, err := s.Pump().StartMux(context.Background())
			if err != nil {
				return nil, err
			}
			ms, err := mux.New(link, mux.DefaultConfig(mux.RoleServer))
			if err != nil {
				return nil, err
			}
			go func() {
				_ = ms.Serve(context.Background(), func(st *mux.Stream) {
					if st.Kind() == mux.KindControl {
						ctlCh <- st
						return
					}
					_ = st.Close()
				})
			}()
			select {
			case ctl := <-ctlCh:
				return ctl, nil
			case <-time.After(15 * time.Second):
				return nil, context.DeadlineExceeded
			}
		})
		chB <- res{sess, err}
	}()

	ra, rb := <-chA, <-chB
	if ra.err != nil {
		t.Fatalf("role A: %v", ra.err)
	}
	if rb.err != nil {
		t.Fatalf("role B: %v", rb.err)
	}
	defer ra.sess.Close()
	defer rb.sess.Close()

	if !ra.sess.Pump().Muxed() {
		t.Fatal("role A is not muxed")
	}

	resFile, err := ra.sess.SendFile(srcPath, nil)
	if err != nil {
		t.Fatalf("SendFile over mux: %v", err)
	}
	if !resFile.OK {
		t.Fatalf("transfer rejected: %s", resFile.Message)
	}

	got, err := os.ReadFile(filepath.Join(dstDir, "payload.bin"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if len(got) != size {
		t.Fatalf("received %d bytes, want %d", len(got), size)
	}
	gotSum := sha256.Sum256(got)
	if hex.EncodeToString(gotSum[:]) != hex.EncodeToString(wantSum[:]) {
		t.Fatal("hash mismatch after transfer over mux")
	}
}

// serveControl accepts and drains non-control streams on the initiator side so
// yamux does not accumulate them.
func serveControl(ms *mux.Session) {
	_ = ms.Serve(context.Background(), func(st *mux.Stream) {
		if st.Kind() != mux.KindControl {
			_ = st.Close()
		}
	})
}
