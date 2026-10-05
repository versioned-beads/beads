package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Help is rendered before graph workspace admission, so it must describe both
// ordinary behavior and the graph preview without opening storage.
func TestGraphPreviewHelpDocumentsPlaytestCommands(t *testing.T) {
	for _, cmd := range []*cobra.Command{
		initCmd, rememberCmd, memoriesCmd, recallCmd, createCmd, showCmd,
		updateCmd, deleteCmd, forgetCmd, depAddCmd, linkCmd, closeCmd,
		reopenCmd, deferCmd, undeferCmd, readyCmd, listCmd, blockedCmd, graphCmd, statusCmd,
		typesCmd, versionsCmd, historyCmd,
	} {
		t.Run(cmd.Name()+"-scope", func(t *testing.T) {
			if out := captureStdout(t, cmd.Help); !strings.Contains(out, "Graph preview workspaces:") {
				t.Fatalf("%s help omits graph preview guidance", cmd.Name())
			}
		})
	}

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		want []string
	}{
		{"remember", rememberCmd, []string{"Graph preview workspaces:", "bd remember 'Revised policy' --update policy", "--if-revision TOKEN", "Omitted fields remain unchanged"}},
		{"create", createCmd, []string{"Graph preview workspaces:", "--bead-type types/preview-memory-v2", "--id policy"}},
		{"link", linkCmd, []string{
			"Graph preview workspaces:", "--link-type types/example-cites", "Memory or Issue",
			// --link-type sits beside the ordinary -t/--type, so its own usage says it is graph-only.
			"Installed Link Type: types/NAME or full local URL (graph preview only)",
			// Informational Links default to the current source; the blocking Type does not.
			"The blocking Type types/preview-blocks-v1", "unlike informational Types, one of",
			"--if-source-revision TOKEN or --unconditional-source.",
		}},
		{"update", updateCmd, []string{"Graph preview workspaces:", "--properties", "--if-revision TOKEN"}},
		{"list", listCmd, []string{"Graph preview workspaces:", "all installed Bead Types", "--bead-type types/NAME", "Issue-specific filters", "newest recorded change first", "hidden unless --all is given", "--all also removes the row limit", "BEADS_MAX_ROWS refuses a page of more Beads", "--sort, --reverse", "Issues only", "a line under the header saying Memories are", "it is simply not"}},
		{"types", typesCmd, []string{"Graph preview workspaces:", "types/NAME", "--details", "--bead-type", "--link-type"}},
		{"versions", versionsCmd, []string{"Graph preview workspaces:", "Use bd versions ID to list one Memory, Issue or Link's retained versions newest", "Bare ID means beads/ID; use links/PATH for a Link.", "--version TOKEN or bd compare ID --from TOKEN --to TOKEN", "BDP HTTP History", "Ordinary Issue workspaces:", "List the versions recorded for a bead by versioned history."}},
		{"history", historyCmd, []string{"Graph preview workspaces:", "bd history ID is an alias for bd versions ID.", "--limit and --events are not supported by the graph alias.", "BDP HTTP History", "Ordinary Issue workspaces:", "Show the complete version history of an issue"}},
		{"defer", deferCmd, []string{"Graph preview workspaces:", "bd defer ID --if-revision TOKEN", "--unconditional", "--until and", "no automatic wake-up", "Ordinary Issue workspaces:"}},
		{"undefer", undeferCmd, []string{"Graph preview workspaces:", "bd undefer ID --if-revision TOKEN", "--unconditional", "no automatic wake-up", "Ordinary Issue workspaces:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, tc.cmd.Help)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("help missing %q: %s", want, out)
				}
			}
		})
	}

	// Versions, history and dateless deferral lead with the graph section, ahead of their long
	// ordinary descriptions (versioned-history recording, Dolt commits).
	for _, cmd := range []*cobra.Command{versionsCmd, historyCmd, deferCmd, undeferCmd} {
		t.Run(cmd.Name()+"-graph-first", func(t *testing.T) {
			out := captureStdout(t, cmd.Help)
			graphAt, ordinaryAt := strings.Index(out, "Graph preview workspaces:"), strings.Index(out, "Ordinary Issue workspaces:")
			if graphAt < 0 || ordinaryAt < 0 || graphAt > ordinaryAt {
				t.Fatalf("%s help must put graph guidance before the ordinary description: %s", cmd.Name(), out)
			}
		})
	}
}
