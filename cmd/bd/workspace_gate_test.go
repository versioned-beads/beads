package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/workspacegate"
)

// resetGateTestEnv pins every env var the physical-root resolver consults so
// a developer machine's beads setup (shared-server mode, custom data dirs,
// central config) cannot change which gates these tests acquire.
func resetGateTestEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"BEADS_DOLT_SERVER_MODE",
		"BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_DATA_DIR",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_PROXIED_SERVER_ROOT_PATH",
		"BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("BEADS_CENTRAL_CONFIG", filepath.Join(t.TempDir(), "no-central.json"))
}

func newGateTestWorkspace(t *testing.T) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"dolt","database":"beads.db","dolt_mode":"embedded"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	return beadsDir
}

func TestCommandNeedsExclusiveGate(t *testing.T) {
	root := &cobra.Command{Use: "bd"}
	backup := &cobra.Command{Use: "backup"}
	backupRestore := &cobra.Command{Use: "restore [path]"}
	backup.AddCommand(backupRestore)
	root.AddCommand(backup)
	// Top-level `bd restore <issue-id>` is an ISSUE restore
	// (cmd/bd/restore.go), not a database restore: it must stay SHARED.
	issueRestore := &cobra.Command{Use: "restore [issue-id]"}
	root.AddCommand(issueRestore)
	list := &cobra.Command{Use: "list"}
	root.AddCommand(list)

	cases := []struct {
		name string
		cmd  *cobra.Command
		want bool
	}{
		{"backup restore is exclusive", backupRestore, true},
		{"top-level issue restore is not", issueRestore, false},
		{"list is not", list, false},
		{"backup parent is not", backup, false},
		{"root is not", root, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandNeedsExclusiveGate(tc.cmd); got != tc.want {
				t.Errorf("commandNeedsExclusiveGate(%s) = %v, want %v", tc.cmd.Name(), got, tc.want)
			}
		})
	}
}

func TestAcquireCommandWorkspaceGatesAbsentWorkspace(t *testing.T) {
	resetGateTestEnv(t)
	t.Cleanup(releaseWorkspaceGates)

	list := &cobra.Command{Use: "list"}
	before := newGateTestWorkspace(t)
	if err := acquireCommandWorkspaceGates(context.Background(), list, before); err != nil || workspaceGateHandle == nil {
		t.Fatalf("initial command must hold a gate: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "nope", ".beads")
	if err := acquireCommandWorkspaceGates(context.Background(), list, missing); err != nil {
		t.Fatalf("absent beadsDir must be silently ungated, got %v", err)
	}
	if workspaceGateHandle != nil {
		t.Error("absent beadsDir must leave no gate handle")
	}
}

func TestAcquireCommandWorkspaceGatesBlockedByExclusiveHolder(t *testing.T) {
	resetGateTestEnv(t)
	t.Cleanup(releaseWorkspaceGates)
	list := &cobra.Command{Use: "list"}
	prior := newGateTestWorkspace(t)
	if err := acquireCommandWorkspaceGates(context.Background(), list, prior); err != nil || workspaceGateHandle == nil {
		t.Fatalf("initial command must hold a gate: %v", err)
	}
	beadsDir := newGateTestWorkspace(t)

	gate, err := workspacegate.ForWorkspace(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := gate.Acquire(context.Background(), workspacegate.Exclusive,
		workspacegate.Options{Reason: "test maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	if err := acquireCommandWorkspaceGates(context.Background(), list, beadsDir); err == nil {
		t.Fatal("SHARED acquisition under a foreign exclusive holder must abort, got nil error")
	}
	if workspaceGateHandle != nil {
		t.Error("failed acquisition must leave no gate handle")
	}
}

func TestAcquireInitMutationGateKeepsReplacementExclusiveDuringPreflight(t *testing.T) {
	resetGateTestEnv(t)
	beadsDir := newGateTestWorkspace(t)
	physicalRoot := filepath.Join(filepath.Dir(beadsDir), "dolt-data")

	oldOnWait := exclusiveGateOnWait
	secondWaited := make(chan struct{}, 1)
	exclusiveGateOnWait = func(string) {
		select {
		case secondWaited <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { exclusiveGateOnWait = oldOnWait })

	firstPreflightEntered := make(chan struct{})
	allowFirstPreflight := make(chan struct{})
	type result struct {
		h   *workspacegate.MultiHandle
		err error
	}
	firstResult := make(chan result, 1)
	go func() {
		h, err := acquireInitMutationGate(context.Background(), beadsDir, physicalRoot, func() error {
			close(firstPreflightEntered)
			<-allowFirstPreflight
			return nil
		})
		firstResult <- result{h: h, err: err}
	}()
	<-firstPreflightEntered

	secondPreflightEntered := make(chan struct{})
	secondResult := make(chan result, 1)
	go func() {
		h, err := acquireInitMutationGate(context.Background(), beadsDir, physicalRoot, func() error {
			close(secondPreflightEntered)
			return nil
		})
		secondResult <- result{h: h, err: err}
	}()

	select {
	case <-secondWaited:
	case <-secondPreflightEntered:
		t.Fatal("second replacement entered preflight while first held the mutation gates")
	}

	close(allowFirstPreflight)
	first := <-firstResult
	if first.err != nil {
		t.Fatalf("first init mutation gate: %v", first.err)
	}
	if err := first.h.Release(); err != nil {
		t.Fatalf("release first init mutation gate: %v", err)
	}

	<-secondPreflightEntered
	second := <-secondResult
	if second.err != nil {
		t.Fatalf("second init mutation gate: %v", second.err)
	}
	if err := second.h.Release(); err != nil {
		t.Fatalf("release second init mutation gate: %v", err)
	}
}

func TestAcquireInitMutationGateReleasesOnPreflightError(t *testing.T) {
	resetGateTestEnv(t)
	beadsDir := newGateTestWorkspace(t)
	physicalRoot := filepath.Join(filepath.Dir(beadsDir), "dolt-data")
	refusal := errors.New("destroy token rejected")

	_, err := acquireInitMutationGate(context.Background(), beadsDir, physicalRoot, func() error {
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("init mutation gate error = %v, want preflight refusal", err)
	}

	h, err := acquireInitMutationGate(context.Background(), beadsDir, physicalRoot, nil)
	if err != nil {
		t.Fatalf("init mutation gate remained held after refusal: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("release init mutation gate: %v", err)
	}
}

func TestReleaseWorkspaceGatesIdempotent(t *testing.T) {
	resetGateTestEnv(t)
	beadsDir := newGateTestWorkspace(t)

	list := &cobra.Command{Use: "list"}
	if err := acquireCommandWorkspaceGates(context.Background(), list, beadsDir); err != nil {
		t.Fatal(err)
	}
	if workspaceGateHandle == nil {
		t.Fatal("expected a held gate handle")
	}
	releaseWorkspaceGates()
	if workspaceGateHandle != nil {
		t.Error("handle must be cleared on release")
	}
	// Second release must be a no-op, not a panic or double-unlock.
	releaseWorkspaceGates()

	// And the gate must actually be free again: an exclusive acquisition
	// succeeds after release.
	gate, err := workspacegate.ForWorkspace(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := gate.Acquire(context.Background(), workspacegate.Exclusive, workspacegate.Options{})
	if err != nil {
		t.Fatalf("gate still held after releaseWorkspaceGates: %v", err)
	}
	_ = h.Release()
}

// The cross-wiring guarantee: a chokepoint SHARED hold (a normal command
// mid-flight) excludes acquireMigrateGates' EXCLUSIVE acquisition on the
// same workspace. Also exercises the nil-rootCtx path inside
// acquireMigrateGates (tests have no process signal context), which used to
// panic before the nil-context normalization.
func TestChokepointSharedExcludesMigrateExclusive(t *testing.T) {
	resetGateTestEnv(t)
	t.Cleanup(releaseWorkspaceGates)
	beadsDir := newGateTestWorkspace(t)

	// rootCtx is a package global that production sets via
	// setupGracefulShutdown() in PersistentPreRunE and cancels via
	// rootCancel() in PersistentPostRunE WITHOUT resetting the var to nil —
	// harmless in production (the process exits), but any earlier in-process
	// test that exercises the full command path (Execute()) leaves rootCtx
	// pointing at an already-canceled context for whatever test runs next in
	// the same binary. acquireMigrateGates now threads rootCtx through to
	// acquireExclusiveWorkspaceGates, so this test is sensitive to that
	// leak: pin it to nil (the documented "no process signal context yet"
	// case this test exercises) regardless of what ran before it.
	oldRootCtx := rootCtx
	rootCtx = nil
	t.Cleanup(func() { rootCtx = oldRootCtx })

	oldWait := exclusiveGateWait
	exclusiveGateWait = 10 * time.Millisecond
	t.Cleanup(func() { exclusiveGateWait = oldWait })

	list := &cobra.Command{Use: "list"}
	if err := acquireCommandWorkspaceGates(context.Background(), list, beadsDir); err != nil {
		t.Fatal(err)
	}
	if workspaceGateHandle == nil {
		t.Fatal("expected a held shared gate handle")
	}

	release, err := acquireMigrateGates(beadsDir, false, "test migrate")
	if err == nil {
		release()
		t.Fatal("migrate EXCLUSIVE acquisition must fail while the chokepoint holds SHARED")
	}

	// After the shared holder releases, the migration proceeds.
	releaseWorkspaceGates()
	release, err = acquireMigrateGates(beadsDir, false, "test migrate")
	if err != nil {
		t.Fatalf("migrate acquisition after shared release: %v", err)
	}
	release()
}

// setReadonlyMode pins strict --readonly for one test, as PersistentPreRunE
// leaves readonlyMode by the time it acquires the gates.
func setReadonlyMode(t *testing.T, on bool) {
	t.Helper()
	original := readonlyMode
	readonlyMode = on
	t.Cleanup(func() { readonlyMode = original })
}

// newGateTestServerWorkspace is newGateTestWorkspace for a local-server
// workspace: its physical root is .beads/dolt, not .beads/embeddeddolt.
func newGateTestServerWorkspace(t *testing.T) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"dolt","database":"beads.db","dolt_mode":"server"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	return beadsDir
}

// dirEntryNames lists dir's immediate entries by name, so a test can prove a
// command left the directory exactly as it found it.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Strict --readonly is mutation-free (effectiveRootStorePolicy), so taking the
// command's gates must not create any gate file: not the workspace gate beside
// .beads, and not the physical-root gate inside it (.beads/embeddeddolt for an
// embedded workspace, .beads/dolt for a local server one).
func TestAcquireCommandWorkspaceGatesStrictReadonlyCreatesNoGateFiles(t *testing.T) {
	layouts := []struct {
		name  string
		build func(*testing.T) string
	}{
		{"embedded", newGateTestWorkspace},
		{"local server", newGateTestServerWorkspace},
	}
	for _, tc := range layouts {
		t.Run(tc.name, func(t *testing.T) {
			resetGateTestEnv(t)
			t.Cleanup(releaseWorkspaceGates)
			setReadonlyMode(t, true)
			beadsDir := tc.build(t)
			projectDir := filepath.Dir(beadsDir)
			projectBefore, beadsBefore := dirEntryNames(t, projectDir), dirEntryNames(t, beadsDir)

			list := &cobra.Command{Use: "list"}
			if err := acquireCommandWorkspaceGates(context.Background(), list, beadsDir); err != nil {
				t.Fatalf("strict readonly acquisition on a never-gated workspace: %v", err)
			}
			if got := dirEntryNames(t, projectDir); !slices.Equal(got, projectBefore) {
				t.Errorf("strict readonly changed %s\n before: %v\n after:  %v", projectDir, projectBefore, got)
			}
			if got := dirEntryNames(t, beadsDir); !slices.Equal(got, beadsBefore) {
				t.Errorf("strict readonly changed %s\n before: %v\n after:  %v", beadsDir, beadsBefore, got)
			}
		})
	}
}

// Not creating a gate file must not mean not honoring one. Once a workspace's
// gate files exist (any normal bd command creates them), a strict readonly
// command still holds them SHARED — so maintenance sees it and refuses to run
// over it — and is still refused while maintenance holds the gate.
func TestAcquireCommandWorkspaceGatesStrictReadonlyStillHonorsExistingGates(t *testing.T) {
	resetGateTestEnv(t)
	t.Cleanup(releaseWorkspaceGates)
	beadsDir := newGateTestWorkspace(t)
	list := &cobra.Command{Use: "list"}
	ctx := context.Background()

	// A normal command materializes the gate files.
	setReadonlyMode(t, false)
	if err := acquireCommandWorkspaceGates(ctx, list, beadsDir); err != nil {
		t.Fatal(err)
	}
	releaseWorkspaceGates()

	setReadonlyMode(t, true)
	if err := acquireCommandWorkspaceGates(ctx, list, beadsDir); err != nil {
		t.Fatalf("strict readonly acquisition against existing gate files: %v", err)
	}
	gate, err := workspacegate.ForWorkspace(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(ctx, workspacegate.Exclusive, workspacegate.Options{}); !errors.Is(err, workspacegate.ErrBusy) {
		t.Fatalf("exclusive acquire while a strict readonly command is running: err = %v, want ErrBusy", err)
	}
	releaseWorkspaceGates()

	holder, err := gate.Acquire(ctx, workspacegate.Exclusive, workspacegate.Options{Reason: "test maintenance"})
	if err != nil {
		t.Fatalf("exclusive acquire after the strict readonly command released: %v", err)
	}
	defer func() { _ = holder.Release() }()
	if err := acquireCommandWorkspaceGates(ctx, list, beadsDir); err == nil {
		t.Fatal("strict readonly acquisition under a foreign exclusive holder must abort, got nil error")
	}
}

// Only STRICT readonly skips creating the gate files. A normal command keeps
// creating them: that is what makes it visible to a maintenance operation that
// starts later (the exclusive acquirer locks the same file), so the first gate
// holder on a workspace cannot go unseen.
func TestAcquireCommandWorkspaceGatesNormalCommandStillCreatesGateFiles(t *testing.T) {
	resetGateTestEnv(t)
	t.Cleanup(releaseWorkspaceGates)
	setReadonlyMode(t, false)
	beadsDir := newGateTestWorkspace(t)

	list := &cobra.Command{Use: "list"}
	if err := acquireCommandWorkspaceGates(context.Background(), list, beadsDir); err != nil {
		t.Fatal(err)
	}
	for _, gateFile := range []string{
		filepath.Join(filepath.Dir(beadsDir), ".beads.gate.lock"),
		filepath.Join(beadsDir, "embeddeddolt.gate.lock"),
	} {
		if _, err := os.Stat(gateFile); err != nil {
			t.Errorf("a normal command must create its gate file: %v", err)
		}
	}
}

// End to end: the real `bd --readonly` leaves no gate file behind. The
// in-process tests above pin acquireCommandWorkspaceGates; this one pins that
// the command line reaches it with strict readonly already resolved.
func TestStrictReadonlyCommandCreatesNoGateFiles(t *testing.T) {
	bdBin := buildBDForInitTests(t)
	beadsDir := newGateTestServerWorkspace(t)
	projectDir := filepath.Dir(beadsDir)

	cmd := exec.Command(bdBin, "--readonly", "list")
	cmd.Dir = projectDir
	cmd.Env = hermeticInitEnv(t.TempDir(), "BEADS_DIR="+beadsDir, "BEADS_DOLT_AUTO_START=0")
	// No Dolt server sits behind this workspace, so the command is expected to
	// fail when it opens the store; only what it left on disk matters here.
	out, _ := cmd.CombinedOutput()

	var gateFiles []string
	for _, dir := range []string{projectDir, beadsDir} {
		for _, name := range dirEntryNames(t, dir) {
			if strings.Contains(name, ".gate.lock") {
				gateFiles = append(gateFiles, filepath.Join(dir, name))
			}
		}
	}
	if len(gateFiles) != 0 {
		t.Fatalf("bd --readonly created gate files %v\noutput: %s", gateFiles, out)
	}
}
