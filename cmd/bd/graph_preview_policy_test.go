//go:build cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGraphPreviewCLIWritePolicy(t *testing.T) {
	bd := buildBDUnderTest(t)
	initArgs := []string{"init", "--graph-mode", "link", "--scope-url", "https://example.invalid/policy/", "--skip-hooks", "--skip-agents", "--non-interactive", "--json"}
	type policyCase struct {
		name  string
		flags []string
		env   []string
		setup func(*testing.T, string, string) func()
		code  string
	}
	configFile := func(workspace bool) func(*testing.T, string, string) func() {
		return func(t *testing.T, work, home string) func() {
			path := filepath.Join(home, ".config", "bd", "config.yaml")
			if workspace {
				path = filepath.Join(work, ".beads", "config.yaml")
			}
			writeFile(t, path, []byte("readonly: true\n"))
			return func() {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	freeze := func(t *testing.T, work, _ string) func() {
		writeFile(t, filepath.Join(work, "mayor", "town.json"), []byte("{}\n"))
		path := filepath.Join(work, "MIGRATION-FREEZE")
		writeFile(t, path, []byte("policy-test\t2026-09-24T00:00:00Z\tgraph write control\n"))
		return func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
	policies := []policyCase{
		{name: "readonly-flag", flags: []string{"--readonly"}, code: "permission_denied"},
		{name: "readonly-env", env: []string{"BD_READONLY=true"}, code: "permission_denied"},
		{name: "readonly-config", setup: configFile(false), code: "permission_denied"},
		{name: "migration-freeze", setup: freeze, code: "permission_denied"},
	}
	initCases := append([]policyCase(nil), policies...)
	for _, flag := range []struct{ name, value string }{
		{"server-host", "127.0.0.1"}, {"server-port", "1"}, {"server-user", "root"},
		{"server-socket", "/missing/policy.sock"}, {"server-tls", "true"},
	} {
		initCases = append(initCases, policyCase{name: "orphan-" + flag.name, flags: []string{"--" + flag.name + "=" + flag.value}, code: "invalid_selector"})
	}
	for _, tc := range initCases {
		t.Run("init/"+tc.name, func(t *testing.T) {
			work, home := t.TempDir(), t.TempDir()
			if tc.setup != nil {
				defer tc.setup(t, work, home)()
			}
			before := legacyUpgradeTreeDigest(t, work)
			graphPolicyCLI(t, bd, work, home, tc.env, tc.code, append(append([]string(nil), initArgs...), tc.flags...)...)
			if after := legacyUpgradeTreeDigest(t, work); after != before {
				t.Fatal("refused init changed workspace")
			}
			if _, err := os.Lstat(filepath.Join(work, ".beads")); !os.IsNotExist(err) {
				t.Fatalf("refused init created .beads: %v", err)
			}
		})
	}

	// One normal embedded init supplies the store. No SQL seeding or fixture
	// schema is used; each refused write is checked from a new CLI process.
	t.Run("remember", func(t *testing.T) {
		work, home := t.TempDir(), t.TempDir()
		graphPolicyCLI(t, bd, work, home, nil, "", initArgs...)
		policies[2].setup = configFile(true)
		for _, tc := range policies {
			t.Run(tc.name, func(t *testing.T) {
				cleanup := func() {}
				if tc.setup != nil {
					cleanup = tc.setup(t, work, home)
				}
				defer cleanup()
				path := "beads/" + tc.name
				args := []string{"remember", "must not persist", "--id", path, "--title", "Refused", "--json"}
				before := legacyUpgradeTreeDigest(t, work)
				graphPolicyCLI(t, bd, work, home, tc.env, tc.code, append(args, tc.flags...)...)
				if after := legacyUpgradeTreeDigest(t, work); after != before {
					t.Fatal("refused remember changed workspace")
				}
				for _, subject := range []string{"beads/plan", "links/context"} {
					patchArgs := []string{"update", subject, "--patch=@" + filepath.Join(work, "missing-patch.json"), "--unconditional", "--json"}
					graphPolicyCLI(t, bd, work, home, tc.env, tc.code, append(patchArgs, tc.flags...)...)
					if after := legacyUpgradeTreeDigest(t, work); after != before {
						t.Fatal("refused patch changed workspace")
					}
				}
				graphPolicyCLI(t, bd, work, home, nil, "not_found", "show", path, "--json")
			})
		}
	})
}

func TestGraphPreviewCorruptMetadataRefusesBeforeLegacyOpening(t *testing.T) {
	bd := buildBDUnderTest(t)
	for _, marker := range []bool{false, true} {
		name := "explicit-assertion"
		if marker {
			name = "persisted-marker"
		}
		t.Run(name, func(t *testing.T) {
			work, home := t.TempDir(), t.TempDir()
			beadsDir := filepath.Join(work, ".beads")
			writeFile(t, filepath.Join(beadsDir, "metadata.json"), []byte("{\n"))
			if marker {
				writeFile(t, filepath.Join(beadsDir, graphPreviewMarker), []byte(graphPreviewGeneration))
			}
			for _, args := range [][]string{
				{"show", "beads/plan", "--json"},
				{"show", "beads/plan", "--version=observed", "--json"},
				{"recall", "beads/plan", "--version=observed", "--json"},
				{"compare", "beads/plan", "--from=observed", "--to=observed", "--json"},
				{"memories", "--format=records-json"},
			} {
				if !marker {
					args = append(args, "--graph-mode", "link")
				}
				before := legacyUpgradeTreeDigest(t, work)
				graphPolicyCLI(t, bd, work, home, nil, "graph_not_initialized", args...)
				if after := legacyUpgradeTreeDigest(t, work); after != before {
					t.Fatalf("corrupt graph metadata refusal changed workspace: %v", args)
				}
			}
		})
	}
}

func TestGraphPreviewInitQuiet(t *testing.T) {
	bd := buildBDUnderTest(t)
	for _, jsonMode := range []bool{false, true} {
		name := "human"
		if jsonMode {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			work, home := t.TempDir(), t.TempDir()
			args := []string{"init", "--graph-mode", "link", "--scope-url", "https://example.invalid/quiet/", "--skip-hooks", "--skip-agents", "--non-interactive", "--quiet"}
			if jsonMode {
				args = append(args, "--json")
			}
			out := graphPolicyCLI(t, bd, work, home, nil, "", args...)
			if jsonMode {
				if !json.Valid([]byte(out)) {
					t.Fatalf("--quiet suppressed structured output: %q", out)
				}
			} else if out != "" {
				t.Fatalf("--quiet emitted human output: %q", out)
			}
			graphPolicyCLI(t, bd, work, home, nil, "", "remember", "Persisted after quiet init", "--id", "beads/quiet", "--title", "Quiet", "--json")
			graphPolicyCLI(t, bd, work, home, nil, "", "show", "beads/quiet", "--json")
		})
	}
}

func TestGraphPreviewRejectsUnsupportedBackend(t *testing.T) {
	bd := buildBDUnderTest(t)
	work, home := t.TempDir(), t.TempDir()
	graphPolicyCLI(t, bd, work, home, nil, "", "init", "--graph-mode", "link", "--scope-url", "https://example.invalid/backend/", "--skip-hooks", "--skip-agents", "--non-interactive", "--json")
	metadataPath := filepath.Join(work, ".beads", "metadata.json")
	original, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"postgres", "mysql", "sqlite", "unsupported-preview"} {
		t.Run(backend, func(t *testing.T) {
			var metadata map[string]any
			if err := json.Unmarshal(original, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata["backend"] = backend
			modified, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, metadataPath, modified)
			before := legacyUpgradeTreeDigest(t, work)
			path := "beads/refused-" + backend
			for _, args := range [][]string{
				{"remember", "must not persist", "--id", path, "--title", "Refused", "--json"},
				{"show", path, "--json"}, {"status", "--graph", "--json"},
			} {
				graphPolicyCLI(t, bd, work, home, nil, "graph_not_initialized", args...)
			}
			if after := legacyUpgradeTreeDigest(t, work); after != before {
				t.Fatal("unsupported backend refusal changed workspace")
			}
			writeFile(t, metadataPath, original)
			graphPolicyCLI(t, bd, work, home, nil, "not_found", "show", path, "--json")
		})
	}
}

func graphPolicyCLI(t *testing.T, bd, work, home string, extraEnv []string, code string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bd, args...)
	cmd.Dir = work
	// A whitelist prevents the operator's selected workspace, routing, policy,
	// credentials and telemetry settings from entering these disposable runs.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"TMPDIR=" + os.TempDir(), "TMP=" + os.TempDir(), "TEMP=" + os.TempDir(),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "missing-gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"BD_DISABLE_METRICS=1", "BD_DISABLE_EVENT_FLUSH=1", "BD_NON_INTERACTIVE=1",
		"BEADS_DOLT_AUTO_START=0", "DOLT_METRICS_DISABLED=1", "NO_COLOR=1",
	}
	if developerDir := os.Getenv("DEVELOPER_DIR"); developerDir != "" {
		cmd.Env = append(cmd.Env, "DEVELOPER_DIR="+developerDir)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("CLI did not complete within deadline: %v\n%s", ctx.Err(), stderr.String())
	}
	if code == "" {
		if err != nil {
			t.Fatalf("graph command %v failed: %v\n%s", args, err, stderr.String())
		}
		return out.String()
	}
	if err == nil {
		t.Fatalf("expected %s refusal; stdout=%s", code, out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("refusal emitted success stdout: %s", out.String())
	}
	var diagnostic struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
		t.Fatalf("expected typed refusal: %v\n%s", err, stderr.String())
	}
	if diagnostic.Code != code || diagnostic.Retryable {
		t.Fatalf("expected non-retryable %s; stderr=%s", code, stderr.String())
	}
	return out.String()
}

// The original C0 admission guarantee also covers deferred mixed-core flags.
func TestGraphPreviewC0DeferredCommandsRefuseBeforeLegacyOpen(t *testing.T) {
	bd := buildBDUnderTest(t)
	work, home := t.TempDir(), t.TempDir()
	graphPolicyCLI(t, bd, work, home, nil, "", "init", "--graph-mode", "link", "--scope-url", "https://example.invalid/c0/", "--skip-hooks", "--skip-agents", "--non-interactive", "--json")
	created := graphPolicyCLI(t, bd, work, home, nil, "", "remember", "C0 body", "--id", "beads/plan", "--title", "Plan", "--json")
	for _, args := range [][]string{
		{"create", "Must refuse", "--estimate=3", "--due=tomorrow", "--defer=tomorrow", "--json"}, {"update", "beads/plan", "--title", "Must refuse", "--priority=1", "--unconditional", "--json"},
		// Native upstream label operations must not enter the graph writer.
		{"label", "rename", "old", "new", "--json"}, {"label", "rename", "old", "new", "--dry-run", "--json"},
		{"update", "beads/plan", "-l", "new", "--unconditional", "--json"},
		{"memories", "--json"}, {"recall", "beads/plan", "--json"},
		{"list", "--json"}, {"ready", "--limit=1", "--json"}, {"close", "beads/plan", "--force", "--json"},
		{"link", "beads/plan", "beads/other", "--type=related", "--json"}, {"serve", "--json"},
		{"db-proxy-child", "--root", filepath.Join(work, ".beads"), "--port", "1", "--backend", "external", "--json"},
	} {
		before := legacyUpgradeTreeDigest(t, work)
		graphPolicyCLI(t, bd, work, home, nil, "capability_unavailable", args...)
		if after := legacyUpgradeTreeDigest(t, work); after != before {
			t.Fatalf("refused %v changed workspace", args)
		}
	}
	first := graphPolicyCLI(t, bd, work, home, nil, "", "show", "beads/plan", "--json")
	second := graphPolicyCLI(t, bd, work, home, nil, "", "show", "beads/plan", "--json")
	if first != created || second != created {
		t.Fatal("new-process current reads differ from creation")
	}
	status := graphPolicyCLI(t, bd, work, home, nil, "", "status", "--graph", "--json")
	var result struct {
		Result struct {
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(status), &result); err != nil {
		t.Fatal(err)
	}
	wantEnabled := map[string]bool{}
	for _, capability := range []string{
		"memoryCreate", "memoryRead", "memoryBodyFileInput", "memoryBodyStdinInput", "memoryPropertiesUpdate",
		"memorySelectedUpdate", "memorySelectedUpdateUnconditional", "memoryOverwriteDisclosure", "memoryUnreferencedDelete", "issueCreate", "issueCreateAuthorship",
		"issueCreateFields", "issueInitialNotes", "issueNotesAppend", "issueEstimateUpdate", "issueReferenceUpdate",
		"issueClaim", "issueUnclaim", "issueTextUpdate", "issuePriorityUpdate", "issueAssigneeUpdate", "issueAssigneeFilter", "issueDueDate", "issueDueFilter", "informationalLink", "blockingDependency", "linkPropertiesUpdate", "linkUnlink", "blockingDependencyUnlink",
		"incidentLinks", "ownedLinks", "issueClose", "issueReopen", "issueDatelessDeferral", "issueReady", "genericRead",
		"issueList", "beadList", "beadTypeFilter", "issueBlocked", "genericTraversal",
		"memoryDiscovery", "memoryBodyRecall", "exactVersionRead", "exactVersionCompare",
		// versionList is `bd versions` (and `bd history` as its graph-mode
		// alias). It sits beside the exactVersion* pair deliberately: those
		// read ONE token, this one enumerates them in order. It is distinct
		// from historyExact, which stays false and describes the HTTP profile.
		"versionList",
		"memoryPropertiesPatch", "linkPropertiesPatch",
	} {
		wantEnabled[capability] = true
		if !result.Result.Capabilities[capability] {
			t.Fatalf("implemented capability missing: %s", capability)
		}
	}
	for capability, enabled := range result.Result.Capabilities {
		if enabled && !wantEnabled[capability] {
			t.Fatalf("unimplemented capability advertised: %s", capability)
		}
	}
}

func TestGraphPreviewGenericFlagsRefuseLegacyOpening(t *testing.T) {
	bd := buildBDUnderTest(t)
	for _, args := range [][]string{
		{"remember", "--update", "beads/plan", "--title", "Refused", "--unconditional"},
		{"link", "demo-one", "demo-two", "--properties", `{}`},
		{"link", "demo-one", "demo-two", "--id", "links/context"},
		{"update", "demo-one", "--properties", `{}`, "--unconditional"},
		{"update", "beads/plan", "--patch=@/missing/patch.json", "--unconditional"},
		{"update", "demo-one", "--if-revision", "observed"},
		{"update", "demo-one", "--if-source-revision", "observed"},
		{"delete", "beads/plan", "--if-revision", "observed"},
		{"delete", "beads/plan", "--unconditional=false"},
		{"forget", "beads/plan", "--if-revision="},
		{"forget", "beads/plan", "--unconditional"},
		{"show", "beads/plan", "--version=observed"},
		{"recall", "beads/plan", "--version="},
		{"compare", "beads/plan", "--from=observed", "--to=observed"},
		{"memories", "--all=false"},
		{"memories", "--details=false"},
		{"memories", "--format=table"},
		{"memories", "--format=records-json"},
	} {
		work, home := t.TempDir(), t.TempDir()
		before := legacyUpgradeTreeDigest(t, work)
		graphPolicyCLI(t, bd, work, home, nil, "capability_unavailable", append(args, "--json")...)
		if after := legacyUpgradeTreeDigest(t, work); after != before {
			t.Fatalf("generic flags changed a legacy workspace: %v", args)
		}
	}
}
