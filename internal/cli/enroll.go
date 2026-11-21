// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// Credential enrollment: the discovery node's admission decision.
//
// This is where a team credential turns into a member. The checks, in
// order, and why each one is here:
//
//  1. The credential must be signed by the ACCOUNT key. The node holds the
//     account public key (--account-pub); without that key it cannot tell a
//     real credential from a made-up one, and it refuses to enroll at all.
//  2. The credential's id must not be on the anchor's revocation list.
//     This check comes before signature and expiry ON PURPOSE: a leaked
//     credential satisfies both right up to its (year-long) expiry, and the
//     revocation is the more actionable truth — the owner said no, and no
//     is not negotiable by re-presenting the string (D77, pre-revocation).
//  3. The credential must not be expired. Expiry bounds how long a leaked
//     credential can be used at all.
//  4. The device certificate must be signed by the credential's DELEGATOR
//     key — not by the account, and not by anyone else.
//  5. The certificate must speak for the same account as the credential.
//  6. The delegation's device quota must not be exhausted. The count comes
//     from the ROSTER, so revoking a device gives its slot back.
//  7. The device must not already be a member with a different key.
//
// The delegation record itself is written into the roster, because that is
// what lets every member verify the delegated certificate (M4-CREDENTIAL.md:
// without it the device is in the network and invisible to everyone).

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
	"github.com/muzimu217/container-mesh/internal/relay"
)

// enrollDevice admits a device that presents a team credential.
//
// The roster is passed in and updated in place; the caller persists it and
// publishes it to the members (the same shape as the certificate adoption
// path, which also mutates the live list before saving).
func enrollDevice(members *identity.MemberList, accountPub ed25519.PublicKey, req discovery.EnrollRequest, now time.Time) discovery.EnrollResponse {
	if len(accountPub) != ed25519.PublicKeySize {
		return discovery.EnrollResponse{OK: false, Message: "this node cannot verify credentials: start it with --account-pub"}
	}
	pub, err := credential.DecodePublic(req.Credential)
	if err != nil {
		return discovery.EnrollResponse{OK: false, Message: "credential is malformed: " + err.Error()}
	}
	// The revocation list, FIRST of the credential checks: the id is part
	// of the account-signed body, so it cannot be swapped by the holder.
	// A listed id is refused no matter how valid the signature is — the
	// owner turned this credential off, possibly before anyone ever used
	// it, and "the signature is fine" is exactly what a leaked credential
	// looks like. The message tells the joiner what happened and what to
	// do, because from where they sit the string looked perfectly good.
	if members.CredentialRevoked(pub.ID) {
		return discovery.EnrollResponse{OK: false, Message: fmt.Sprintf(
			"credential %s has been revoked; ask the owner for a new one", pub.ID)}
	}
	// The credential's own signature, checked against the ACCOUNT key.
	if err := pub.Verify(accountPub, now); err != nil {
		return discovery.EnrollResponse{OK: false, Message: "credential rejected: " + err.Error()}
	}
	// The certificate must be the delegator's, for this account.
	cert := req.Cert
	if len(cert.SignerPub) == 0 {
		return discovery.EnrollResponse{OK: false, Message: "certificate is not signed by a delegator"}
	}
	if !cert.SignerPub.Equal(pub.DelegatorPub) {
		return discovery.EnrollResponse{OK: false, Message: "certificate was signed by a key this credential does not authorise"}
	}
	if cert.AccountID != pub.AccountID {
		return discovery.EnrollResponse{OK: false, Message: "certificate belongs to another account"}
	}
	if err := cert.VerifySigner(pub.DelegatorPub); err != nil {
		return discovery.EnrollResponse{OK: false, Message: "certificate rejected: " + err.Error()}
	}
	if cert.Expired(now) {
		return discovery.EnrollResponse{OK: false, Message: "certificate expired at " + cert.ExpiresAt.UTC().Format(time.RFC3339)}
	}
	// Quota: counted from the roster, so revoking a device frees its slot.
	if used := members.DelegatedCount(pub.DelegatorPub); used >= pub.MaxDevices {
		return discovery.EnrollResponse{OK: false, Message: fmt.Sprintf(
			"credential quota exhausted: %d of %d devices already joined with it", used, pub.MaxDevices)}
	}
	// The account must be the one this node serves.
	if members.AccountID != pub.AccountID {
		return discovery.EnrollResponse{OK: false, Message: "credential is for account " + string(pub.AccountID) + ", this network is " + string(members.AccountID)}
	}
	// The delegation must be on record: it is what every other member
	// verifies the certificate against.
	//
	// The record IS the credential's public half — already account-signed,
	// with the canonical form every member checks. Nothing is re-assembled
	// here: a rebuilt struct would carry no valid signature (this node holds
	// no account key), and an unsigned record would make the device invisible
	// to every member.
	if existing, ok := members.DelegationFor(pub.DelegatorPub); ok {
		// Already known. A credential that grants LESS than the record must
		// tighten it: the account re-issued a narrower authorisation.
		if pub.MaxDevices < existing.MaxDevices {
			if err := members.AddDelegation(*pub); err != nil {
				return discovery.EnrollResponse{OK: false, Message: "delegation rejected: " + err.Error()}
			}
		}
	} else if err := members.AddDelegation(*pub); err != nil {
		return discovery.EnrollResponse{OK: false, Message: "delegation rejected: " + err.Error()}
	}
	if err := members.Add(cert); err != nil {
		return discovery.EnrollResponse{OK: false, Message: "device rejected: " + err.Error()}
	}
	return discovery.EnrollResponse{OK: true, DeviceID: cert.DeviceID, Message: "enrolled"}
}

// installEnrollHandler wires the credential enrollment endpoint into the
// discovery node.
//
// It refuses to install the handler without the account public key: a node
// that cannot verify a credential must not accept one, and must not answer
// enrollment requests at all.
func installEnrollHandler(srv *relay.Server, membersPath string, members *identity.MemberList, accountPub []byte) {
	if len(accountPub) != ed25519.PublicKeySize {
		fmt.Fprintf(os.Stderr, "credential enrollment disabled: --account-pub is required to verify a credential\n")
		return
	}
	account := ed25519.PublicKey(accountPub)
	srv.SetEnrollHandler(func(req discovery.EnrollRequest) discovery.EnrollResponse {
		// Resolve the LIVE roster on every request, not the one captured at
		// startup: watchApprovals replaces the MemberList object whenever it
		// hot-reloads the file, so a captured pointer would keep honouring a
		// delegation the owner revoked from another process (`cmesh cred
		// revoke` writes the file; the reload lands in a NEW list) — and the
		// next enrollment would then re-save its stale view over the file,
		// erasing the revocation. MeshMembers() is the list the anchor
		// currently enforces, hot reload included; the startup pointer is
		// only a fallback for the window before discovery was enabled.
		live := srv.MeshMembers()
		if live == nil {
			live = members
		}
		res := enrollDevice(live, account, req, time.Now().UTC())
		if !res.OK {
			fmt.Fprintf(os.Stderr, "enrollment refused: %s\n", res.Message)
			return res
		}
		// Persist before publishing: a member that is announced but not on
		// disk would vanish on the next reload of the members file.
		//
		// The MERGED save is the point: the live list can be up to a couple
		// of seconds behind the file (the anchor reloads it on a tick), and
		// a plain save would write that stale view back over the file —
		// erasing a revocation the operator had just recorded and letting
		// the revoked credential enrol again. Measured on the real
		// machine: two revoked ids in the file, one enrolment, and the
		// revocation list was gone.
		if err := live.SaveMembersMerged(membersPath); err != nil {
			fmt.Fprintf(os.Stderr, "enrollment failed to persist: %v\n", err)
			return discovery.EnrollResponse{OK: false, Message: "could not persist the roster: " + err.Error()}
		}
		// The device must still be on the roster AFTER the save.
		//
		// The merged save drops the members whose credential was
		// revoked, and a credential revoked in the last couple of
		// seconds (the window between the anchor's hot reload and this
		// request) is exactly the case where the device this request
		// just admitted is dropped by its own save.
		//
		// Without this check the anchor would answer "enrolled", log
		// "enrolled device X", and the device would silently vanish —
		// a joiner that believes it joined and is not a member.
		if !live.Contains(res.DeviceID) {
			fmt.Fprintf(os.Stderr, "enrollment of %s was dropped by the save: its credential has been revoked\n", res.DeviceID)
			return discovery.EnrollResponse{OK: false, Message: "credential has been revoked; ask the owner for a new one"}
		}
		srv.SetMeshMembers(live)
		fmt.Fprintf(os.Stderr, "enrolled device %s under a team credential (roster v%d)\n",
			res.DeviceID, live.CurrentVersion())
		return res
	})
}
