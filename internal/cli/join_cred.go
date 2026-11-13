// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package cli

// The client half of credential enrollment: `cmesh join --cred <string>`.
//
// This is the ONE command a team member runs. It replaces the old dance
// (init → show-pub → owner signs → owner hands the certificate over →
// owner drops it into the anchor's directory → join), which needed the
// account owner for every single device.
//
// The order matters:
//  1. the device identity must exist (it is created here if it does not —
//     the credential is the authority, not a human);
//  2. the certificate is minted HERE, with the credential's delegator key,
//     so the delegator's private key never leaves this machine;
//  3. the certificate is ENROLLED at the discovery node before the first
//     announce — an announce from a device the node has never heard of is
//     rejected, so enrolling first is what makes the announce work;
//  4. only then does the normal join run (announce + members stream).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/muzimu217/container-mesh/internal/credential"
	"github.com/muzimu217/container-mesh/internal/discovery"
	"github.com/muzimu217/container-mesh/internal/identity"
)

// enrollTimeout bounds one enrollment attempt. The node is right there on
// the network; a minute is generous.
const enrollTimeout = 60 * time.Second

// enrollWithCredential admits this device at the discovery node.
//
// The credential carries the seed addresses, so the joiner knows where to
// go: one string is enough. Every address is tried in turn; the first node
// that accepts the device wins.
func enrollWithCredential(ctx context.Context, cred *credential.Credential, dev *identity.Device) error {
	pub, err := cred.PublicPart()
	if err != nil {
		return err
	}
	// The certificate lives no longer than the credential: a device must
	// not outlive the authority that vouched for it.
	ttl := cred.Public.ExpiresAt.Sub(time.Now().UTC())
	if cred.Public.ExpiresAt.Before(time.Now().UTC()) {
		return fmt.Errorf("credential expired at %s", cred.Public.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if max := credential.DefaultCertTTL; ttl > max {
		ttl = max
	}
	cert, err := cred.MintCert(dev.PublicKey(), ttl, time.Now().UTC())
	if err != nil {
		return err
	}
	body, err := discovery.EncodeEnrollRequest(discovery.EnrollRequest{
		Credential: pub,
		Cert:       *cert,
	})
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: enrollTimeout}
	var lastErr error
	for _, addr := range cred.Public.DiscoveryAddrs {
		base, err := discovery.NormalizeDiscoveryURL(addr)
		if err != nil {
			lastErr = err
			continue
		}
		url := base + "/mesh/enroll"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", addr, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		var res discovery.EnrollResponse
		if err := json.Unmarshal(raw, &res); err != nil {
			lastErr = fmt.Errorf("%s: unreadable answer (HTTP %d)", addr, resp.StatusCode)
			continue
		}
		if res.OK {
			return nil
		}
		// The node answered with a reason — that is the verdict, not a
		// transport problem: trying the next address would only repeat it.
		return fmt.Errorf("enrollment refused: %s", res.Message)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no discovery address in the credential answered")
	}
	return fmt.Errorf("could not enroll at the discovery node: %w", lastErr)
}

// ensureDeviceIdentity returns the device identity, creating it when the
// machine has none yet.
//
// Creating it here is what makes the credential a ONE-command join: the
// device key is this machine's own, the credential is the authority to
// introduce it, and no human needs to touch either.
func ensureDeviceIdentity(dir, passphrase string) (*identity.Device, error) {
	path := identity.IdentityPath(dir, identity.DeviceFileName)
	if _, err := os.Stat(path); err == nil {
		return identity.LoadDevice(path, passphrase)
	}
	dev, err := identity.Generate()
	if err != nil {
		return nil, fmt.Errorf("could not create a device identity: %w", err)
	}
	if err := dev.Save(path, passphrase); err != nil {
		return nil, fmt.Errorf("could not save the device identity: %w", err)
	}
	fmt.Fprintf(os.Stderr, "created device identity %s\n", dev.ID())
	return dev, nil
}

// inviteFromCredential turns a credential into the invite the join flow
// already understands: the same account, the same discovery addresses.
func inviteFromCredential(cred *credential.Credential) discovery.Invite {
	return discovery.Invite{
		AccountID:      cred.Public.AccountID,
		AccountPub:     cred.Public.AccountPub,
		DiscoveryAddrs: cred.Public.DiscoveryAddrs,
		IssuedAt:       cred.Public.IssuedAt,
	}
}

var _ ed25519.PublicKey
