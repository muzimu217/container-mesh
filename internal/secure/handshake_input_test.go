package secure

import "testing"

// TestHandshakeRejectsMalformedInput reproduces the crash a malicious relay can
// trigger: it completes the WebSocket upgrade and then sends a binary "null"
// as the first handshake message. pake.Update unmarshals it into a *Pake,
// gets nil, and dereferences it.
func TestHandshakeRejectsMalformedInput(t *testing.T) {
	payloads := map[string][]byte{
		"json null": []byte("null"),
		"empty obj": []byte("{}"),
		"array":     []byte("[]"),
		"zero byte": {0x00},
		"role only": []byte(`{"Role":0}`),
		"huge":      make([]byte, 1<<20),
	}

	for name, p := range payloads {
		t.Run(name, func(t *testing.T) {
			h, err := NewHandshake("test-code-123", RoleB)
			if err != nil {
				t.Fatal(err)
			}
			// Must return an error, not panic.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on malformed handshake input: %v", r)
				}
			}()
			if _, _, err := h.Step(p); err == nil {
				t.Error("malformed input was accepted")
			}
		})
	}
}
