// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/quic-go/quic-go"
)

// TestClassifyAuthFailure: a crypto failure (the peer rejected our
// certificate, or our pin rejected theirs) is an AUTH failure, never an
// "unreachable" one. This distinction decides whether a path fallback may
// happen: an auth failure must NOT be retried over another path, because
// the credentials are wrong, not the network.
//
// The sample is a real quic.TransportError with a crypto error code
// (0x100 + tls alert 42 = bad certificate), the exact shape the peer
// produces when it rejects our certificate. quic.Dial itself returns nil
// error in that case; the error surfaces at the ack read stage, so the
// classifier must see it there.
func TestClassifyAuthFailure(t *testing.T) {
	cryptoErr := &quic.TransportError{
		Remote:       true,
		ErrorCode:    quic.TransportErrorCode(0x100 + 42), // tls: bad certificate
		ErrorMessage: "tls: bad certificate",
	}
	got := classifyDialError(fmt.Errorf("read ack: %w", cryptoErr))
	if !errors.Is(got, ErrAuth) {
		t.Fatalf("a crypto failure must classify as ErrAuth, got %v", got)
	}
	if errors.Is(got, ErrUnreachable) {
		t.Fatalf("a crypto failure must NOT also classify as ErrUnreachable, got %v", got)
	}
}

// TestClassifyUnreachable: a peer that simply does not answer is
// unreachable, and must NOT be confused with an auth failure.
func TestClassifyUnreachable(t *testing.T) {
	got := classifyDialError(fmt.Errorf("quic dial 192.168.2.226:9: %w", context.DeadlineExceeded))
	if !errors.Is(got, ErrUnreachable) {
		t.Fatalf("a dial timeout must classify as ErrUnreachable, got %v", got)
	}
	if errors.Is(got, ErrAuth) {
		t.Fatalf("a dial timeout must NOT classify as ErrAuth, got %v", got)
	}
}

// TestClassifyMutuallyExclusive walks a table of every error shape the
// dial path can produce and asserts that no sample is classified as BOTH
// ErrAuth and ErrUnreachable — the two classes that drive opposite
// fallback decisions. A classifier that returns both would make the
// fallback decision ambiguous.
func TestClassifyMutuallyExclusive(t *testing.T) {
	samples := []struct {
		name string
		err  error
	}{
		{"crypto remote", &quic.TransportError{Remote: true, ErrorCode: quic.TransportErrorCode(0x100 + 42)}},
		{"crypto local", &quic.TransportError{Remote: false, ErrorCode: quic.TransportErrorCode(0x100 + 42)}},
		{"context deadline", context.DeadlineExceeded},
		{"net timeout", &net.OpError{Op: "dial", Err: &timeoutError{}}},
		{"quic handshake timeout", &quic.HandshakeTimeoutError{}},
		{"quic idle timeout", &quic.IdleTimeoutError{}},
		{"plain error", errors.New("boom")},
		{"nil", nil},
	}
	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			got := classifyDialError(s.err)
			if got == nil {
				return // nothing to classify
			}
			auth := errors.Is(got, ErrAuth)
			unreach := errors.Is(got, ErrUnreachable)
			if auth && unreach {
				t.Fatalf("error %v classified as BOTH ErrAuth and ErrUnreachable", got)
			}
		})
	}
}

// TestClassifyUnreachableOnHandshakeTimeout: quic-go's own timeout types
// are the "nobody answered" signal and must land on the unreachable side.
func TestClassifyUnreachableOnHandshakeTimeout(t *testing.T) {
	for _, err := range []error{&quic.HandshakeTimeoutError{}, &quic.IdleTimeoutError{}} {
		got := classifyDialError(err)
		if !errors.Is(got, ErrUnreachable) {
			t.Fatalf("%v must classify as ErrUnreachable, got %v", err, got)
		}
		if errors.Is(got, ErrAuth) {
			t.Fatalf("%v must NOT classify as ErrAuth, got %v", err, got)
		}
	}
}

// TestClassifyRefused: the listener answered with a refusal verdict — the
// peer reached us and said no. That is neither unreachable nor an auth
// failure of the transport; it is a refusal.
func TestClassifyRefused(t *testing.T) {
	got := classifyDialError(fmt.Errorf("listener refused: %s", "no inbox configured"))
	if !errors.Is(got, ErrRefused) {
		t.Fatalf("a listener refusal must classify as ErrRefused, got %v", got)
	}
	if errors.Is(got, ErrAuth) || errors.Is(got, ErrUnreachable) {
		t.Fatalf("a listener refusal must not be auth or unreachable, got %v", got)
	}
}

// TestClassifyKeepsOriginalText: the classifier ADDS a class, it does not
// rewrite the message. The original text is what the operator reads.
func TestClassifyKeepsOriginalText(t *testing.T) {
	orig := errors.New("quic dial 10.0.0.1:7000: connection refused")
	got := classifyDialError(orig)
	if !strings.Contains(got.Error(), "quic dial 10.0.0.1:7000: connection refused") {
		t.Fatalf("the original text must survive classification, got %q", got.Error())
	}
}

// TestDialNonMemberIsAuth: dialing a device that is not in the member view
// fails with an auth-classified error AND keeps the original "not a member"
// text. The two must coexist: the text is the operator-facing contract,
// the class is the machine-facing one.
func TestDialNonMemberIsAuth(t *testing.T) {
	f := newDirectFixture(t)
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	_, err = DialDirect(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    stranger.ID(),
		Endpoints: []string{"127.0.0.1:1"},
		Kind:      KindPing,
	})
	if err == nil {
		t.Fatal("dialing a non-member must fail")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("a non-member target must classify as ErrAuth, got %v", err)
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("the original membership reason must survive, got %q", err.Error())
	}
}

// TestDialNoEndpointsIsNoRoute: a member with no announced endpoint is a
// routing problem, NOT an auth problem — the credentials were never
// exercised. Getting this wrong would make the path layer give up on a
// reachable-by-relay target.
func TestDialNoEndpointsIsNoRoute(t *testing.T) {
	f := newDirectFixture(t)
	_, err := DialDirect(context.Background(), DialConfig{
		Device:    f.bob,
		Resolver:  MemberListAdapter{List: f.members},
		Target:    f.aline.ID(),
		Endpoints: nil,
		Kind:      KindPing,
	})
	if err == nil {
		t.Fatal("dialing a target with no endpoints must fail")
	}
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("a target with no endpoints must classify as ErrNoRoute, got %v", err)
	}
	if errors.Is(err, ErrAuth) {
		t.Fatalf("a target with no endpoints must NOT classify as ErrAuth, got %v", err)
	}
	if !strings.Contains(err.Error(), "no reachable endpoints") {
		t.Fatalf("the original no-endpoint reason must survive, got %q", err.Error())
	}
}

// TestDialUnreachableIsUnreachable: a target whose endpoint does not
// answer must be classified as unreachable end-to-end — including through
// the "all N endpoints failed" aggregate.
func TestDialUnreachableIsUnreachable(t *testing.T) {
	f := newDirectFixture(t)
	dead := deadEndpoint(t)
	_, err := DialDirect(context.Background(), DialConfig{
		Device:           f.bob,
		Resolver:         MemberListAdapter{List: f.members},
		Target:           f.aline.ID(),
		Endpoints:        []string{dead},
		Kind:             KindPing,
		HandshakeTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("dialing a dead endpoint must fail")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a dead endpoint must classify as ErrUnreachable, got %v", err)
	}
}

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
