package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/graphstore"
)

func memoryDeleteCommand(t *testing.T, flags []string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	for _, name := range []string{"if-revision", "from-file"} {
		cmd.Flags().String(name, "", "")
	}
	for _, name := range []string{"unconditional", "force", "dry-run", "cascade", "erase"} {
		cmd.Flags().Bool(name, false, "")
	}
	if err := cmd.ParseFlags(flags); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestGraphPreviewMemoryDeleteAdmission(t *testing.T) {
	oldConfig, oldJSON, oldStructured := graphPreviewConfig, jsonOutput, graphPreviewStructuredErrors
	t.Cleanup(func() {
		graphPreviewConfig, jsonOutput, graphPreviewStructuredErrors = oldConfig, oldJSON, oldStructured
	})
	graphPreviewConfig = &configfile.Config{GraphScopeURL: "https://example.invalid/delete/"}
	jsonOutput, graphPreviewStructuredErrors = false, false
	for _, tc := range []struct {
		name, selector string
		flags          []string
		forget         bool
		preview        bool
		code           int
	}{
		{"preview", "beads/plan", nil, false, true, 0},
		{"explicit-preview", "beads/plan", []string{"--force=false"}, false, true, 0},
		{"guarded-preview", "beads/plan", []string{"--if-revision=observed"}, false, true, 0},
		{"guarded-apply", "https://example.invalid/delete/beads/plan", []string{"--force", "--if-revision=observed"}, false, false, 0},
		{"unconditional-apply", "beads/plan", []string{"--force", "--unconditional"}, false, false, 0},
		{"forget", "beads/plan", []string{"--if-revision=observed"}, true, false, 0},
		{"forget-unconditional", "beads/plan", []string{"--unconditional"}, true, false, 0},
		{"missing-apply-guard", "beads/plan", []string{"--force"}, false, false, 2},
		{"missing-forget-guard", "beads/plan", nil, true, false, 2},
		{"both-guards", "beads/plan", []string{"--if-revision=observed", "--unconditional"}, false, false, 2},
		{"empty-guard", "beads/plan", []string{"--if-revision="}, false, false, 2},
		{"false-unconditional", "beads/plan", []string{"--unconditional=false"}, false, false, 2},
		{"invalid-guard", "beads/plan", []string{"--if-revision=\xff"}, false, false, 2},
		{"oversized-guard", "beads/plan", []string{"--if-revision=" + strings.Repeat("x", graphstore.PreviewVersionTokenLimit+1)}, false, false, 2},
		{"foreign", "https://foreign.invalid/beads/plan", nil, false, false, 2},
		{"link", "links/context", nil, false, false, 2},
		{"bare-bead-id", "plan", nil, false, true, 0},
		{"cascade", "beads/plan", []string{"--cascade"}, false, false, 5},
		{"false-cascade", "beads/plan", []string{"--cascade=false"}, false, false, 5},
		{"file", "beads/plan", []string{"--from-file=/unread/deletions"}, false, false, 5},
		{"dry-run", "beads/plan", []string{"--dry-run"}, false, false, 5},
		{"erase", "beads/plan", []string{"--erase"}, false, false, 5},
		{"forget-force", "beads/plan", []string{"--force", "--unconditional"}, true, false, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := graphPreviewMemoryDeleteInput(memoryDeleteCommand(t, tc.flags), []string{tc.selector}, tc.forget)
			if tc.code != 0 {
				var failure *exitError
				if !errors.As(err, &failure) || failure.Code != tc.code || got != (graphstore.MemoryDeleteRequest{}) {
					t.Fatalf("request=%+v refusal=%v", got, err)
				}
				return
			}
			if err != nil || got.Path != "beads/plan" || got.Preview != tc.preview || got.Actor != "" {
				t.Fatalf("request=%+v error=%v", got, err)
			}
		})
	}
	for _, args := range [][]string{nil, {"beads/plan", "beads/other"}} {
		if _, err := graphPreviewMemoryDeleteInput(memoryDeleteCommand(t, nil), args, false); err == nil {
			t.Fatalf("admitted batch/missing selector: %v", args)
		}
	}
}

func TestGraphPreviewMemoryDeleteReadonlyBeforeOpening(t *testing.T) {
	oldConfig, oldReadonly, oldJSON := graphPreviewConfig, readonlyMode, jsonOutput
	t.Cleanup(func() { graphPreviewConfig, readonlyMode, jsonOutput = oldConfig, oldReadonly, oldJSON })
	graphPreviewConfig = &configfile.Config{GraphScopeURL: "https://example.invalid/delete/"}
	readonlyMode, jsonOutput = true, true
	for _, forget := range []bool{false, true} {
		flags := []string{"--unconditional"}
		if !forget {
			flags = append(flags, "--force")
		}
		var err error
		stderr := captureStderr(t, func() {
			err = runGraphPreviewDeleteMemory(memoryDeleteCommand(t, flags), []string{"beads/plan"}, forget)
		})
		var failure *exitError
		if !errors.As(err, &failure) || failure.Code != 5 {
			t.Fatalf("readonly deletion reached store: %v", err)
		}
		var diagnostic struct{ Code string }
		if err := json.Unmarshal([]byte(stderr), &diagnostic); err != nil || diagnostic.Code != "permission_denied" {
			t.Fatalf("readonly refusal lost: %s (%v)", stderr, err)
		}
	}
}

func TestGraphPreviewMemoryDeletePolicyError(t *testing.T) {
	oldJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = oldJSON })
	jsonOutput = true
	var err error
	stderr := captureStderr(t, func() {
		err = graphStorageError(fmt.Errorf("delete: %w", graphstore.ErrIncidentLinkConstraint))
	})
	var failure *exitError
	if !errors.As(err, &failure) || failure.Code != 4 {
		t.Fatalf("incident Link refusal lost constraint status: %v", err)
	}
	var diagnostic struct {
		Code      string
		Retryable bool
	}
	if err := json.Unmarshal([]byte(stderr), &diagnostic); err != nil || diagnostic.Code != "constraint_violation" || diagnostic.Retryable {
		t.Fatalf("typed non-retryable deletion policy refusal lost: %s (%v)", stderr, err)
	}
}
