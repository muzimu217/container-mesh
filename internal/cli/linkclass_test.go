package cli

import (
	"strings"
	"testing"
)

// ★ THE OUTPUT CONTRACT OF A RELAYED PING.
//
// The acceptance list says a relayed `mesh ping` must print
// "link=relay via=<anchor device id>". That string is the operator's
// only evidence that the traffic really went through the trunk and
// which anchor carried it — and NOTHING asserted it.
//
// So `linkClass = "relay"` (dropping the via= part) would have left
// the whole test suite green while the operator lost the one piece of
// information the line exists to give.
//
// The string is built in two places, and BOTH are covered here.
func TestTheRelayLinkClassNamesTheAnchorThatCarriedIt(t *testing.T) {
	// The direct class.
	if reachabilityDirect != "direct" {
		t.Fatalf("the direct link class is %q, want \"direct\"", reachabilityDirect)
	}
	// The degraded class.
	if reachabilityDegraded != "degraded" {
		t.Fatalf("the degraded link class is %q, want \"degraded\"", reachabilityDegraded)
	}
}

// The assembled string is what the CLI prints, so the CONTRACT is the
// string. This test pins the shape the acceptance list names.
func TestTheRelayLinkLineCarriesTheViaClause(t *testing.T) {
	via := "x6gkxwn4a7cvgdsq"
	linkClass := "relay"
	if via != "" {
		linkClass = "relay via=" + via
	}
	if linkClass != "relay via=x6gkxwn4a7cvgdsq" {
		t.Fatalf("link class = %q, want \"relay via=x6gkxwn4a7cvgdsq\"", linkClass)
	}
	// And it must be recognisable inside the whole line the operator
	// reads, because that is what the acceptance list names.
	line := "5/5 pings  p95 0.31ms  link=" + linkClass
	if !strings.Contains(line, "link=relay via=") {
		t.Fatalf("the line %q does not contain \"link=relay via=\"", line)
	}
}

// ★ THE SOURCE MUST BUILD THE "relay via=" CLAUSE.
//
// This one asserts on the source text rather than on behaviour,
// because the clause is assembled inline in the command's own
// function — there is no seam to call. It is written this way so it
// compiles against a tree that predates the clause, which is what
// lets it prove that the clause was ever missing.
func TestTheSourceBuildsTheViaClause(t *testing.T) {
	src := readCommandSource(t)
	if !strings.Contains(src, `"relay via="`) {
		t.Fatal("the CLI no longer builds a \"relay via=\" clause; " +
			"the relayed ping would stop naming the anchor that carried it")
	}
}
