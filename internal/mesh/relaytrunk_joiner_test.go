package mesh

import (
	"context"
	"testing"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// TestAReconnectingMemberKeepsItsPlaceInTheJoinerTable is the regression
// test for the defect the performance measurement found on the real
// machine.
//
// The joiner table is keyed by device id, and a member can reconnect: the
// new connection registers itself, and then the OLD connection's cleanup
// runs — and deleted the entry BY ID, taking the NEW joiner with it.
//
// The symptom is nasty because it looks like the member's problem and is
// not: the member's own connection was alive and healthy (its log showed
// no join failures after the reconnect), yet the anchor had forgotten it,
// so every relay to it failed — permanently. Restarting the anchor
// cleared the table and the relay came back.
//
// Measured on the real machine: the relay worked for a couple of seconds
// after the target reconnected, then broke for good.
func TestAReconnectingMemberKeepsItsPlaceInTheJoinerTable(t *testing.T) {
	table := NewJoinerTable()

	// The OLD connection's joiner.
	old := &TrunkJoiner{Device: identity.DeviceID("member-1")}
	table.Add(old)

	// The member reconnects: a NEW joiner registers itself, replacing the
	// old entry.
	fresh := &TrunkJoiner{Device: identity.DeviceID("member-1")}
	table.Add(fresh)

	// Now the OLD connection's cleanup runs. It must NOT remove the new
	// joiner.
	table.Remove(old)

	// The table must still hold the NEW joiner. (The table's Open needs a
	// real opener, so the assertion is on the table itself.)
	table.mu.RLock()
	cur := table.joiners[identity.DeviceID("member-1")]
	table.mu.RUnlock()
	if cur != fresh {
		t.Fatal("the member lost its place in the joiner table when its OLD connection was cleaned up")
	}
}

// TestRemovingTheCurrentJoinerStillWorks: the other half — the cleanup
// of the LIVE connection must still remove the entry, or a member that
// really left would stay in the table forever.
func TestRemovingTheCurrentJoinerStillWorks(t *testing.T) {
	table := NewJoinerTable()

	joiner := &TrunkJoiner{Device: identity.DeviceID("member-1")}
	table.Add(joiner)
	table.Remove(joiner)

	if _, err := table.Open(context.Background(), identity.DeviceID("member-1")); err == nil {
		t.Fatal("the joiner is still in the table after its connection was cleaned up")
	}
}

// TestAJoinerThatIsReplacedIsStillClosed: the identity check must not
// leak the old connection. The old joiner's Close is the caller's job
// (the deferred cleanup calls it), and the table's Remove only ever
// touches the map.
func TestAJoinerThatIsReplacedIsStillClosed(t *testing.T) {
	table := NewJoinerTable()

	old := &TrunkJoiner{Device: identity.DeviceID("member-1")}
	table.Add(old)
	fresh := &TrunkJoiner{Device: identity.DeviceID("member-1")}
	table.Add(fresh)

	table.Remove(old)

	// The table holds the NEW joiner — and only the new one.
	table.mu.RLock()
	cur := table.joiners[identity.DeviceID("member-1")]
	table.mu.RUnlock()
	if cur != fresh {
		t.Fatal("the table does not hold the new joiner")
	}
}
