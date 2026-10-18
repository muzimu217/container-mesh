package wsconn

import "crypto/tls"

// insecureTLSConfig is used only when the operator explicitly opts in to
// skipping certificate verification for a lab relay.
func insecureTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}
