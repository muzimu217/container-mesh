package main

import (
	"testing"
)

// parseReleasesForTest reads the release API's JSON list and fails the
// test if it does not parse.
func parseReleasesForTest(t *testing.T, body string) []release {
	t.Helper()
	releases, err := parseReleases([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return releases
}

// ★ THE "latest" PATH MUST HAVE A FALLBACK.
//
// GitHub's /releases/latest points at the newest NON-prerelease. While
// the only release is a prerelease — which is where this project is —
// that path is a 404, and the default install fails outright: a new
// user following the README gets nothing at all.
//
// Measured against the real repository: /releases/latest/download/
// SHA256SUMS → 404, while /releases/download/v1.0.0-rc2/SHA256SUMS →
// 200. So the "latest" URL cannot be the only way in.
//
// The shell installer (packaging/install.sh) falls back to the release
// API. This test pins the Go installer's resolution order: the newest
// release that is neither a draft nor a prerelease, and if there is
// none, the newest prerelease. Both are "a release" — and that is the
// order GitHub's own UI uses.
func TestResolveLatestReleasePrefersAStableRelease(t *testing.T) {
	body := `[
	  {"tag_name":"v1.0.0-rc3","draft":false,"prerelease":true},
	  {"tag_name":"v1.0.0","draft":false,"prerelease":false},
	  {"tag_name":"v0.9.0","draft":false,"prerelease":false}
	]`
	got := pickInstallableRelease(parseReleasesForTest(t, body))
	if got != "v1.0.0" {
		t.Fatalf("resolved = %q, want v1.0.0 (the newest non-prerelease)", got)
	}
}

func TestResolveLatestReleaseFallsBackToAPrerelease(t *testing.T) {
	body := `[
	  {"tag_name":"v1.0.0-rc3","draft":false,"prerelease":true},
	  {"tag_name":"v1.0.0-rc2","draft":false,"prerelease":true}
	]`
	got := pickInstallableRelease(parseReleasesForTest(t, body))
	if got != "v1.0.0-rc3" {
		t.Fatalf("resolved = %q, want v1.0.0-rc3 (the newest prerelease when there is no stable one)", got)
	}
}

func TestResolveLatestReleaseSkipsDrafts(t *testing.T) {
	body := `[
	  {"tag_name":"v2.0.0","draft":true,"prerelease":false},
	  {"tag_name":"v1.0.0-rc2","draft":false,"prerelease":true}
	]`
	got := pickInstallableRelease(parseReleasesForTest(t, body))
	if got != "v1.0.0-rc2" {
		t.Fatalf("resolved = %q, want v1.0.0-rc2 (a draft is not a release)", got)
	}
}

func TestResolveLatestReleaseWithNoUsableRelease(t *testing.T) {
	body := `[{"tag_name":"v2.0.0","draft":true,"prerelease":true}]`
	if got := pickInstallableRelease(parseReleasesForTest(t, body)); got != "" {
		t.Fatalf("resolved = %q, want empty when every entry is a draft", got)
	}
}
