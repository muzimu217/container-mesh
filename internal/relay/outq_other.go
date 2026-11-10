// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

//go:build !linux

package relay

import (
	"fmt"
	"net"
)

// socketOutq is Linux-only (SIOCOUTQ); other platforms report zero and the
// send-queue monitor stays dormant. Dev machines (darwin) never kick on
// queue depth; production members are Linux containers, which is where the
// issue #8 backlog shape was reproduced.
func socketOutq(c net.Conn) (int, error) {
	return 0, fmt.Errorf("relay: send-queue introspection is linux-only")
}
