// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package discovery

import "encoding/json"

// jsonMarshal is json.Marshal with the error swallowed at the call sites that
// construct signature payloads. Every type marshaled there is composed of
// strings, []string and time.Time — none of which can fail — so a nil result
// is impossible in practice, and the signature path has no way to surface an
// error anyway (it returns bytes, not bytes-and-error).
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
