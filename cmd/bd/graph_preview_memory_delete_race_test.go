//go:build cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/graphstore"
)

type graphDeleteRaceReceipt struct {
	stdout string
	code   string
	pid    int
}

// Both commands start before either Wait. This proves independent installed CLI
// callers, not forced SQL overlap; the storage tests provide that server proof.
// Do not accept infrastructure failures as a losing mutation.
func graphDeleteRacePair(t *testing.T, bd, work, home string, args [2][]string) [2]graphDeleteRaceReceipt {
	t.Helper()
	return graphMutationRacePair(t, bd, work, home, args, map[string]int{"revision_conflict": 4, "gone": 3, "constraint_violation": 4})
}

// Each caller supplies only its admitted domain refusals. Keep deletion's
// established outcomes unchanged when exercising another installed mutation.
func graphMutationRacePair(t *testing.T, bd, work, home string, args [2][]string, allowedRefusals map[string]int) [2]graphDeleteRaceReceipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var commands [2]*exec.Cmd
	var stdout, stderr [2]bytes.Buffer
	var receipts [2]graphDeleteRaceReceipt
	var waitErrors [2]error
	var waited [2]bool
	started := 0
	defer func() {
		cancel()
		for i := 0; i < started; i++ {
			if !waited[i] {
				_ = commands[i].Wait()
			}
		}
	}()
	for i := range commands {
		commands[i] = exec.CommandContext(ctx, bd, args[i]...)
		commands[i].Dir = work
		// Match graphPolicyCLI's isolated environment. No operator-selected
		// workspace, credentials, routing or telemetry policy is inherited.
		commands[i].Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + home,
			"TMPDIR=" + os.TempDir(), "TMP=" + os.TempDir(), "TEMP=" + os.TempDir(),
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
			"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "missing-gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
			"BD_DISABLE_METRICS=1", "BD_DISABLE_EVENT_FLUSH=1", "BD_NON_INTERACTIVE=1",
			"BEADS_DOLT_AUTO_START=0", "DOLT_METRICS_DISABLED=1", "NO_COLOR=1",
		}
		if developerDir := os.Getenv("DEVELOPER_DIR"); developerDir != "" {
			commands[i].Env = append(commands[i].Env, "DEVELOPER_DIR="+developerDir)
		}
		commands[i].Stdout, commands[i].Stderr = &stdout[i], &stderr[i]
		commands[i].WaitDelay = 5 * time.Second
		if err := commands[i].Start(); err != nil {
			t.Fatalf("start race command %v: %v", args[i], err)
		}
		started++
		receipts[i].pid = commands[i].Process.Pid
	}
	for i := range commands {
		waitErrors[i] = commands[i].Wait()
		waited[i] = true
	}
	if ctx.Err() != nil {
		t.Fatalf("race deadline: %v; stderr=%q / %q", ctx.Err(), stderr[0].String(), stderr[1].String())
	}
	if receipts[0].pid <= 0 || receipts[0].pid == receipts[1].pid {
		t.Fatalf("race did not launch distinct processes: %+v", receipts)
	}
	for i, err := range waitErrors {
		if err == nil {
			var envelope struct {
				SchemaVersion int             `json:"schemaVersion"`
				Preview       bool            `json:"preview"`
				Result        json.RawMessage `json:"result"`
			}
			if decodeErr := json.Unmarshal(stdout[i].Bytes(), &envelope); decodeErr != nil || envelope.SchemaVersion != 1 || !envelope.Preview || len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
				t.Fatalf("invalid race success: decode=%v stdout=%s stderr=%s", decodeErr, stdout[i].String(), stderr[i].String())
			}
			receipts[i].stdout = stdout[i].String()
			t.Logf("race command pid=%d args=%v exit=0", receipts[i].pid, args[i])
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || stdout[i].Len() != 0 {
			t.Fatalf("race infrastructure/non-envelope failure: %v stdout=%s stderr=%s", err, stdout[i].String(), stderr[i].String())
		}
		var diagnostic struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		}
		if decodeErr := json.Unmarshal(stderr[i].Bytes(), &diagnostic); decodeErr != nil || diagnostic.Retryable == nil || *diagnostic.Retryable || diagnostic.Message == "" {
			t.Fatalf("race did not return a complete non-retryable refusal: %v stderr=%s", decodeErr, stderr[i].String())
		}
		expectedExit := allowedRefusals[diagnostic.Code]
		if expectedExit == 0 || exit.ExitCode() != expectedExit {
			t.Fatalf("race failed outside admitted mutation outcomes: exit=%d stderr=%s", exit.ExitCode(), stderr[i].String())
		}
		receipts[i].code = diagnostic.Code
		t.Logf("race command pid=%d args=%v exit=%d code=%s", receipts[i].pid, args[i], exit.ExitCode(), diagnostic.Code)
	}
	return receipts
}

func TestGraphPreviewMemoryDeleteConcurrentProcesses(t *testing.T) {
	port := os.Getenv("BEADS_GRAPH_TEST_SERVER_PORT")
	if port == "" {
		t.Skip("set BEADS_GRAPH_TEST_SERVER_PORT for independent ordinary-server CLI processes")
	}
	bd := buildBDUnderTest(t)
	work, home := t.TempDir(), t.TempDir()
	const scope = "https://example.invalid/delete-race/"
	call := func(test *testing.T, args ...string) string {
		test.Helper()
		return graphPolicyCLI(test, bd, work, home, nil, "", append(args, "--json")...)
	}
	call(t, "init", "--graph-mode", "link", "--scope-url", scope, "--skip-hooks", "--skip-agents", "--non-interactive",
		"--server", "--external", "--server-host", "127.0.0.1", "--server-port", port, "--server-user", "root")
	issue := graphMixedResult[graphstore.IssueRecord](t, call(t, "create", "Unchanged Issue source", "--id", "beads/work"))
	issueShown := call(t, "show", "beads/work")

	// The public CLI does not advertise exact History. Read-only inspection of
	// the normally initialized store verifies retention without seeding state or
	// adding a private CLI escape hatch. Authoring remains installed CLI only.
	cfg, err := configfile.LoadForDiscovery(filepath.Join(work, ".beads"))
	if err != nil || cfg == nil || cfg.DoltMode != configfile.DoltModeServer || cfg.GraphScopeURL != scope {
		t.Fatalf("normal init did not publish the expected server binding: %v", err)
	}
	inspectCtx, inspectCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer inspectCancel()
	inspection, err := graphstore.OpenExisting(inspectCtx, graphstore.Options{
		Backend: cfg.DoltMode, Database: cfg.DoltDatabase, Branch: "main",
		Binding: graphstore.Binding{WorkspaceID: cfg.GraphWorkspace, ScopeURL: cfg.GraphScopeURL,
			AuthorityID: cfg.GraphAuthorityID, SchemaVersion: cfg.GraphSchemaVersion},
		ServerHost: cfg.DoltServerHost, ServerPort: cfg.DoltServerPort, ServerUser: cfg.DoltServerUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := inspection.Close(); err != nil {
			t.Error(err)
		}
	}()
	issueRetained, err := inspection.ReadVersion(inspectCtx, "beads/work", issue.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"memory-update", "incoming-link"} {
		t.Run(other, func(t *testing.T) {
			path := "beads/" + other
			original := graphMixedResult[graphstore.Record](t, call(t, "remember", "Original body", "--id", path, "--title", "Original title"))
			before, err := inspection.ReadVersion(inspectCtx, path, original.Version)
			if err != nil || !reflect.DeepEqual(before, original) {
				t.Fatalf("initial retained predecessor: %+v %v", before, err)
			}
			commands := [2][]string{
				{"delete", path, "--force", "--if-revision", original.Revision, "--json"},
				{"remember", "--update", path, "--title", "Winning edit", "--if-revision", original.Revision, "--json"},
			}
			if other == "incoming-link" {
				commands[1] = []string{"link", "beads/work", path, "--id", "links/incoming", "--resource-type", scope + "types/preview-related-v2", "--json"}
			}
			results := graphDeleteRacePair(t, bd, work, home, commands)
			deleteWon, otherWon := results[0].code == "", results[1].code == ""
			if deleteWon == otherWon {
				t.Fatalf("expected exactly one accepted mutation: delete=%+v other=%+v", results[0], results[1])
			}
			// Narrow accepted losers by operation, not merely the generic set of
			// graph errors. In particular, outcome_unknown is not success proof.
			if !deleteWon && results[0].code != "revision_conflict" && !(other == "incoming-link" && results[0].code == "constraint_violation") {
				t.Fatalf("unexpected deletion refusal: %s", results[0].code)
			}
			if !otherWon && results[1].code != "revision_conflict" && results[1].code != "gone" {
				t.Fatalf("unexpected competing mutation refusal: %s", results[1].code)
			}
			finalLive := original
			var link graphstore.LinkRecord
			if deleteWon {
				deleted := graphMixedResult[graphstore.MemoryDeleteResult](t, results[0].stdout)
				if !deleted.Deleted || deleted.Preview || !reflect.DeepEqual(deleted.Memory, original) {
					t.Fatalf("deletion lost final live predecessor: %+v", deleted)
				}
				graphPolicyCLI(t, bd, work, home, nil, "gone", "show", path, "--json")
				if other == "incoming-link" {
					graphPolicyCLI(t, bd, work, home, nil, "not_found", "show", "links/incoming", "--json")
				}
			} else {
				current := graphMixedResult[graphstore.Record](t, call(t, "show", path))
				if other == "memory-update" {
					updated := graphMixedResult[graphstore.MemoryMutationResult](t, results[1].stdout)
					if !updated.Changed || updated.Replaced != nil || updated.Memory.Revision == original.Revision || updated.Memory.ID != original.ID || updated.Memory.Type != original.Type || updated.Memory.Version != updated.Memory.Revision || updated.Memory.Properties.Title != "Winning edit" || updated.Memory.Properties.Body != original.Properties.Body || !reflect.DeepEqual(updated.Memory.Owned, original.Owned) || !reflect.DeepEqual(current, updated.Memory) {
						t.Fatalf("incomplete guarded edit winner: %+v current=%+v", updated, current)
					}
					finalLive = updated.Memory
				} else {
					added := graphMixedResult[graphstore.LinkMutationResult](t, results[1].stdout)
					link = graphMixedResult[graphstore.LinkRecord](t, call(t, "show", "links/incoming"))
					if !added.Changed || !reflect.DeepEqual(added.Link, link) || link.ID != scope+"links/incoming" || link.Source != issue.ID || link.Target != original.ID || !reflect.DeepEqual(current, original) {
						t.Fatalf("incomplete incoming Link winner: %+v current=%+v", added, current)
					}
					incident := graphMixedResult[[]graphstore.LinkRecord](t, call(t, "links", path))
					if len(incident) != 1 || !reflect.DeepEqual(incident[0], link) {
						t.Fatalf("incomplete incident state: %+v", incident)
					}
				}
			}
			// Full current-state validation must not hide an orphan Link or a
			// deleted endpoint. Retained state must survive either race winner.
			if _, err := inspection.CurrentSnapshot(inspectCtx); err != nil {
				t.Fatalf("race left inconsistent inventory: %v", err)
			}
			if retained, err := inspection.ReadVersion(inspectCtx, path, original.Version); err != nil || !reflect.DeepEqual(retained, before) {
				t.Fatalf("race changed retained predecessor: %+v %v", retained, err)
			}
			if call(t, "show", "beads/work") != issueShown {
				t.Fatal("race changed the unowned Issue source")
			}
			// Establish absence in every scheduling outcome, only after the race
			// assertions. Explicit unlink/forget are separate normal operations.
			if !deleteWon {
				if other == "incoming-link" {
					call(t, "unlink", "links/incoming", "--if-revision", link.Revision)
				}
				call(t, "forget", path, "--if-revision", finalLive.Revision)
			}
			graphPolicyCLI(t, bd, work, home, nil, "gone", "show", path, "--json")
			graphPolicyCLI(t, bd, work, home, nil, "identity_reserved", "remember", "Cannot reuse", "--id", path, "--title", "Reserved", "--json")
			if retained, err := inspection.ReadVersion(inspectCtx, path, original.Version); err != nil || !reflect.DeepEqual(retained, before) {
				t.Fatalf("final absence lost original snapshot: %+v %v", retained, err)
			}
			if retained, err := inspection.ReadVersion(inspectCtx, path, finalLive.Version); err != nil || !reflect.DeepEqual(retained, finalLive) {
				t.Fatalf("final absence lost final live snapshot: %+v %v", retained, err)
			}
			if retained, err := inspection.ReadVersion(inspectCtx, "beads/work", issue.Version); err != nil || !reflect.DeepEqual(retained, issueRetained) {
				t.Fatalf("race changed Issue retained state: %+v %v", retained, err)
			}
		})
	}
}
