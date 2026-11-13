// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

// The credential enrollment wire format.
//
// A device that joins under a team credential cannot be admitted by the
// usual route: the account owner is not there to sign its certificate. The
// credential carries a DELEGATOR key instead, and the device presents a
// certificate signed by that delegator together with the credential's
// PUBLIC half.
//
// The secret half never travels: the delegator's private key stays with
// the credential holder, so the discovery node can verify what the delegator
// signed but can never sign anything itself.

import (
	"encoding/json"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// EnrollRequest is the POST /mesh/enroll body.
type EnrollRequest struct {
	// Credential is the credential's PUBLIC half, base64url — the same
	// encoding its own PublicPart method produces.
	Credential string `json:"credential"`
	// Cert is the device certificate the delegator signed.
	Cert identity.DeviceCert `json:"cert"`
}

// EnrollResponse is what the discovery node answers with.
type EnrollResponse struct {
	OK bool `json:"ok"`
	// Message carries the reason when OK is false. It is written for the
	// person operating the device: "quota exhausted" and "signature
	// invalid" lead to completely different actions.
	Message string `json:"message,omitempty"`
	// DeviceID is the enrolled device, so the client can confirm which
	// identity it just obtained.
	DeviceID identity.DeviceID `json:"device_id,omitempty"`
}

// EnrollRequestSizeLimit bounds the enrollment body. A credential plus a
// certificate is well under a kilobyte; the limit exists so a malformed
// peer cannot make the node allocate without bound.
const EnrollRequestSizeLimit = 64 << 10

// EncodeEnrollRequest renders an enrollment request for the wire.
func EncodeEnrollRequest(req EnrollRequest) ([]byte, error) {
	return json.Marshal(req)
}

// DecodeEnrollRequest parses an enrollment request.
func DecodeEnrollRequest(b []byte) (*EnrollRequest, error) {
	var req EnrollRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	return &req, nil
}
