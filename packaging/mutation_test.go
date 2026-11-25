// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★ THE TESTS MUST ACTUALLY GUARD THE FIXES.
//
// A test that stays green when the fix is reverted is not testing the
// fix — it is testing something else, and the fix it appears to cover
// can be undone without anyone noticing.
//
// So each fix is MUTATED back to the behaviour it had before, and the
// matching test MUST go red. This file is that verification, and it runs
// it for real.
//
// ★ WHY IT WORKS ON A COPY.
//
// The mutation REVERTS the implementation — so the tree under test is
// temporarily broken. Running it in place would mean that an
// interrupted run leaves the repository in a broken state, and that a
// failing mutation looks like a failing build.
//
// So the mutation is applied to a COPY of the tree, and the copy is
// what gets tested. The working tree is never touched.
//
// ★ WHY IT IS OPT-IN.
//
// Each mutation runs `go test`, which takes seconds, and there are a
// dozen of them. Making that the default cost would slow every
// contributor's `go test ./...` for a check that only matters when a
// fix is being changed.
//
// So it runs when CMESH_MUTATION_CHECK=1, and SKIPS with an
// explanation otherwise. The skip is loud on purpose: a silently
// skipped check is a check nobody runs.

// mutation is one reverted fix and the test that must catch it.
type mutation struct {
	// name identifies the mutation in the report.
	name string
	// file is the path of the file to mutate, relative to the repo root.
	file string
	// old is the text to look for.
	old string
	// new is the text to replace it with.
	new string
	// extra is an optional second edit, applied after the first.
	extraFile string
	extraOld  string
	extraNew  string
	// testPackage is the package to test.
	testPackage string
	// testName is the -run pattern.
	testName string
}

// mutations are the reverted fixes and the tests that must catch them.
func mutations() []mutation {
	return []mutation{
		{
			name: "install-root-traversal-shell",
			file: "packaging/install.sh",
			old: `chmod 0755 "$INSTALL_DIR" 2>/dev/null || true
chmod 0755 "$INSTALL_DIR/versions" 2>/dev/null || true
mkdir -p "$INSTALL_DIR/versions" || die "cannot create install directory $INSTALL_DIR"
chmod 0755 "$INSTALL_DIR" 2>/dev/null || true`,
			new: `chmod 0755 "$INSTALL_DIR/versions" 2>/dev/null || true
mkdir -p "$INSTALL_DIR/versions" || die "cannot create install directory $INSTALL_DIR"`,
			testPackage: "./packaging/",
			testName:    "TestTheInstallTreeIsTraversable|TestTheInstallRootIsNotOwnerOnly",
		},
		{
			name: "install-root-traversal-go",
			file: "tools/cmesh-install/main.go",
			old: `	for _, dir := range []string{root, versions} {
		if err := os.Chmod(dir, 0o755); err != nil {
			return fmt.Errorf("cannot make %s traversable: %w", dir, err)
		}
	}`,
			new:         `	_ = versions`,
			testPackage: "./tools/cmesh-install/",
			testName:    "TestTheInstalledTreeIsTraversable",
		},
		{
			name: "uninstall-report-purge-go",
			file: "tools/cmesh-install/main.go",
			old: `		reportUninstall(stopped, state, true)
		return nil`,
			new:         `		return nil`,
			testPackage: "./tools/cmesh-install/",
			testName:    "TestThePurgeNamesTheUnitsItStopped",
		},
		{
			name:        "uninstall-retained-state-go",
			file:        "tools/cmesh-install/main.go",
			old:         `	fmt.Printf("uninstalled binaries; runtime state was retained at %s\n", state)`,
			new:         `	fmt.Printf("uninstalled binaries\n")`,
			testPackage: "./tools/cmesh-install/",
			testName:    "TestTheUninstallNamesTheStateItRetained",
		},
		{
			name:        "uninstall-purge-state-shell",
			file:        "packaging/install.sh",
			old:         `            say "purged install root and runtime state: $STATE_DIR"`,
			new:         `            say "purged install root and runtime state"`,
			testPackage: "./packaging/",
			testName:    "TestThePurgeNamesTheStateItRemoved",
		},
		{
			name:        "anchor-exec-condition",
			file:        "packaging/systemd/cmesh-discovery.service",
			old:         `ExecCondition=/bin/sh -c 'if [ -z "$${CMESH_ACCOUNT_PUB}" ]`,
			new:         `#ExecCondition=/bin/sh -c 'if [ -z "$${CMESH_ACCOUNT_PUB}" ]`,
			testPackage: "./packaging/",
			testName:    "TestTheAnchorRefusesToStartWithoutItsLoadBearingConfiguration",
		},
		{
			name:        "member-exec-condition",
			file:        "packaging/systemd/cmesh-member.service",
			old:         `ExecCondition=/bin/sh -c 'if [ -z "$${CMESH_STATE}" ]`,
			new:         `#ExecCondition=/bin/sh -c 'if [ -z "$${CMESH_STATE}" ]`,
			testPackage: "./packaging/",
			testName:    "TestTheMemberRefusesToStartWithoutItsStateDirectory",
		},
		{
			name: "journal-routing",
			file: "packaging/systemd/cmesh-discovery.service",
			old: `StandardOutput=journal
StandardError=journal
SyslogIdentifier=cmesh-discovery`,
			new:         `# StandardOutput=journal`,
			testPackage: "./packaging/",
			testName:    "TestTheUnitsStateTheirLogDestination",
		},
		{
			name: "rollback-floor-go",
			file: "tools/cmesh-install/main.go",
			old: `	if err := checkUnitCompatibility(bin); err != nil {
		return fmt.Errorf("refusing to roll back to %s: %w", version, err)
	}`,
			new:         `	_ = checkUnitCompatibility`,
			testPackage: "./tools/cmesh-install/",
			testName:    "TestTheRollbackRefusesAVersionTheUnitsCannotDrive",
		},
		{
			name:        "rollback-floor-shell",
			file:        "packaging/install.sh",
			old:         `    check_unit_compat "$rollback_bin" || die "refusing to roll back to $ROLLBACK_VERSION: it is below the rollback floor"`,
			new:         `    :`,
			testPackage: "./packaging/",
			testName:    "TestTheRollbackRefusesAVersionTheUnitsCannotDrive",
		},
		{
			name:        "floor-list-matches-units",
			file:        "packaging/install.sh",
			old:         `CMESH_UNIT_FLAGS="serve-discovery:account-pub serve-discovery:addr serve-discovery:announce serve-discovery:panel serve-discovery:panel-token-file serve-discovery:relay-trunk serve-discovery:state join:cred-file join:listen join:relay-trunk join:relay-trunk-id join:state"`,
			new:         `CMESH_UNIT_FLAGS="serve-discovery:panel-token-file join:cred-file"`,
			testPackage: "./packaging/",
			testName:    "TestTheInstallerFloorListMatchesTheShippedUnits",
		},
		{
			name:        "execcondition-no-backtick",
			file:        "packaging/systemd/cmesh-discovery.service",
			old:         `Put the key printed by: cmesh account show --state /var/lib/cmesh`,
			new:         "Put the key from `cmesh account show --state /var/lib/cmesh`",
			testPackage: "./packaging/",
			testName:    "TestNoExecConditionMessageContainsShellCommandSubstitution",
		},
		{
			name: "no-execstartpre",
			file: "packaging/systemd/cmesh-member.service",
			old: `[Service]
Type=simple
User=cmesh
Group=cmesh`,
			new: `[Service]
Type=simple
User=cmesh
Group=cmesh
ExecStartPre=/bin/true`,
			testPackage: "./packaging/",
			testName:    "TestNoUnitUsesExecStartPreForAPrecondition",
		},
		{
			name: "startlimit-in-unit-section",
			file: "packaging/systemd/cmesh-member.service",
			old: `StartLimitIntervalSec=60
StartLimitBurst=5

[Service]`,
			new:       `[Service]`,
			extraFile: "packaging/systemd/cmesh-member.service",
			extraOld: `[Service]
Type=simple`,
			extraNew: `[Service]
StartLimitIntervalSec=60
StartLimitBurst=5
Type=simple`,
			testPackage: "./packaging/",
			testName:    "TestTheStartLimitLivesInTheUnitSection",
		},
	}
}

// TestEveryFixIsGuardedByATest mutates each fix back to its old
// behaviour and requires the matching test to go RED.
//
// A mutation that SURVIVES is the important failure: it means the test
// named in the report does not actually guard the fix, and the report
// would be claiming a guarantee that does not exist.
func TestEveryFixIsGuardedByATest(t *testing.T) {
	if os.Getenv("CMESH_MUTATION_CHECK") != "1" {
		t.Skip("the mutation check reverts each fix and re-runs the suite; " +
			"run it with CMESH_MUTATION_CHECK=1")
	}

	root := repoRoot(t)
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			copyRepoForMutation(t, root, dir)

			applyMutation(t, filepath.Join(dir, m.file), m.old, m.new)
			if m.extraFile != "" {
				applyMutation(t, filepath.Join(dir, m.extraFile), m.extraOld, m.extraNew)
			}

			cmd := exec.Command("go", "test", m.testPackage, "-run", m.testName, "-count=1")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the mutation SURVIVED: %s stayed GREEN after the fix "+
					"was reverted, so the test does not guard it.\n--- output ---\n%s",
					m.testName, out)
			}
			if !strings.Contains(string(out), "FAIL") {
				t.Fatalf("the mutation did not fail the way a test failure should "+
					"(the command failed for another reason): %v\n--- output ---\n%s",
					err, out)
			}
			t.Logf("the mutation was caught: %s went RED as it must", m.testName)
		})
	}
}

// applyMutation replaces text in a file, failing the test when the text
// is not there.
//
// A mutation whose target is missing did not happen — and a mutation
// that did not happen would make the check pass for the wrong reason.
func applyMutation(t *testing.T, path, old, new string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatalf("the mutation target is not in %s:\n%s", path, old)
	}
	out := strings.Replace(string(raw), old, new, 1)
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// copyRepoForMutation copies the parts of the repository a mutation
// test needs.
//
// The heavy directories (the release staging tree, the web assets, the
// git objects) are skipped: they are large, they are not compiled by
// the packages under test, and copying them would make the check slow
// for no benefit.
func copyRepoForMutation(t *testing.T, src, dst string) {
	t.Helper()
	skip := map[string]bool{
		".git": true, "dist": true, "web": true, "out": true,
		"node_modules": true, "marketing": true, ".zcode": true,
	}
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		top := strings.SplitN(rel, string(os.PathSeparator), 2)[0]
		if skip[top] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy the repository: %v", err)
	}
}
