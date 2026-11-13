// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// F42-2: a one-shot receiver must TERMINATE when the local side cannot
// store the file.
//
// The one-shot path waits for exactly one inbound file verdict before it
// exits. A local failure (read-only volume, no space, a path that is not a
// directory) used to end the transfer goroutine without producing a
// verdict, so the receiver sat there until the session timed out — the
// operator saw a hang, not an error.
//
// The receive path now reports a local failure through the same callback as
// a verdict, so the one-shot wait ends with the reason.

import (
	"errors"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/wire"
)

// failingTransfer is an inbound transfer whose local side cannot store the
// file.
type failingTransfer struct {
	name string
	err  error
}

func (f *failingTransfer) Accept(dir string, progress func(sent, total int64)) (*wire.FileResult, error) {
	return nil, f.err
}

func (f *failingTransfer) OfferInfo() wire.FileOffer {
	return wire.FileOffer{Name: f.name}
}

// TestLocalFailureReachesOneShotReceiver: the receive path must hand a
// local failure to OnFile, so the one-shot wait ends with the reason.
func TestLocalFailureReachesOneShotReceiver(t *testing.T) {
	verdict := make(chan wire.FileResult, 1)
	node := NewNode(NodeConfig{
		OutDir: t.TempDir(),
		OnFile: func(res *wire.FileResult, _ string) {
			select {
			case verdict <- *res:
			default:
			}
		},
	})

	cause := errors.New("receiver cannot store the file: permission denied")
	go node.acceptInbound(&failingTransfer{name: "payload.bin", err: cause})

	select {
	case res := <-verdict:
		if res.OK {
			t.Fatal("the verdict claims success on a local failure")
		}
		if res.Message == "" {
			t.Fatal("the verdict carries no reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no verdict reached the one-shot receiver: it would hang until the session times out")
	}
}
