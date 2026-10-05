package main

import "github.com/spf13/cobra"

// Cobra renders help before graph workspace admission. Keep the ordinary help
// intact and add an explicitly scoped section for commands reused by the graph
// preview, so their legacy descriptions do not mislead graph operators.
func init() {
	// These commands have long ordinary-mode descriptions. Put the graph
	// distinction first so help does not lead graph users to turn on legacy
	// recording or expect Dolt commits and unsupported flags.
	versionsCmd.Short = "List retained graph Resource versions or recorded ordinary Bead versions"
	versionsCmd.Long = `Graph preview workspaces:
Use bd versions ID to list one Memory, Issue or Link's retained versions newest
first. Bare ID means beads/ID; use links/PATH for a Link. Every new graph
Resource has a creation version; do not enable ordinary versioned-history
recording for this command. Cite the opaque version token with bd show ID
--version TOKEN or bd compare ID --from TOKEN --to TOKEN. The store-local
ordinal only orders versions. --json returns the version rows. BDP HTTP History
is not available.

Ordinary Issue workspaces:
` + versionsCmd.Long
	historyCmd.Short = "Show graph Resource versions or ordinary Issue commit history"
	historyCmd.Long = `Graph preview workspaces:
bd history ID is an alias for bd versions ID. It lists retained Memory, Issue
or Link versions newest first; bare ID means beads/ID and Link IDs use
links/PATH. Use the opaque token for exact reads, not the store-local ordinal.
--limit and --events are not supported by the graph alias. BDP HTTP History
is not available.

Ordinary Issue workspaces:
` + historyCmd.Long
	deferCmd.Long = `Graph preview workspaces:
Use bd defer ID --if-revision TOKEN to put one unassigned open Issue in the
dateless deferred state. Use --unconditional instead to explicitly accept the current
revision. A repeated defer is a no-op after guard checking. --until and
--reason are unavailable; there is no automatic wake-up. Claimed and
in-progress Issues refuse until their release policy is settled.

Ordinary Issue workspaces:
` + deferCmd.Long
	undeferCmd.Long = `Graph preview workspaces:
Use bd undefer ID --if-revision TOKEN to return one unassigned deferred Issue to open.
Use --unconditional instead to explicitly accept the current revision. A
repeated undefer is a no-op after guard checking; one Issue is changed per
command and there is no automatic wake-up.

Ordinary Issue workspaces:
` + undeferCmd.Long
	for _, entry := range []struct {
		cmd  *cobra.Command
		text string
	}{
		{initCmd, `Use bd init --graph-mode link --scope-url URL in a fresh workspace.
The Scope URL names local identities; it does not start a web server.
--server --external selects an ordinary shared Dolt server; otherwise storage
is embedded. Existing .beads directories are never adopted or overwritten.`},
		{rememberCmd, `Create a Memory with bd remember 'Policy text' [--id policy]
[--title 'Policy']. An omitted ID is generated; an omitted title summarizes the
body. Explicit duplicate IDs fail. Bare policy means canonical beads/policy.
Graph Memories use canonical IDs, not legacy keys.

Update an existing Memory with:
  bd remember 'Revised policy' --update policy
  bd remember --update policy --title 'New title'
Omitted fields remain unchanged. Use --body-file PATH or --stdin instead of
positional body text. --update is required for an existing Memory; --id is
creation-only. The current revision is accepted by default; add
--if-revision TOKEN to reject a stale update. Read the token with
bd show policy --json. --unconditional explicitly selects the default.`},
		{memoriesCmd, `Search Memory titles and bodies with bd memories [SEARCH].
Use --all for a complete bounded result, --details for version/Link counts,
or --format records-json for machine-readable summaries. --json is unavailable;
use bd recall ID for the exact body.`},
		{recallCmd, `Use bd recall ID for one exact Memory body. --version TOKEN
selects a retained body; use bd show ID --json for the record. Graph
recall does not accept legacy keys or --json.`},
		{createCmd, `Create an Issue by default, with an optional --id ID.
Use --bead-type types/preview-memory-v2 to create a Memory instead:
  bd create --bead-type types/preview-memory-v2 --id policy --body 'Code flow policy'
Use bd types to see Bead Types installed in this workspace. Both types/NAME
and full local Type URLs work. --type remains the Issue classification
(for example task or bug), not the Bead Type. Memory creation
accepts body/description/message and an optional title; unsupported Issue-only
fields refuse.`},
		{showCmd, `Use bd show ID (equivalent to beads/ID) for a current Memory or
Issue; use bd show links/ID for a Link. --version TOKEN selects one exact
retained record; this is not
an ordered history listing. --json returns the experimental graph record.`},
		{updateCmd, `Use bd remember --update ID for selected Memory title/body
edits. For complete Memory or informational Link property replacement, use:
  bd update policy --properties '{"title":"Policy","body":"Text"}' --if-revision TOKEN
--patch applies ordered property operations. Generic updates require
--if-revision TOKEN or --unconditional. Informational Links owned by a Memory
may also use --if-source-revision TOKEN; without it, the current source is
accepted. Blocking Dependency properties are not editable here. Issue scalar
edits and standalone --claim are separate graph operations.`},
		{deleteCmd, `For an unreferenced Memory, bd delete ID previews the
deletion without writing. Apply with --force and either --if-revision TOKEN or
--unconditional. Referenced Memories refuse; graph deletion does not cascade.`},
		{forgetCmd, `Use bd forget ID to delete one unreferenced Memory now.
Supply --if-revision TOKEN or --unconditional. Canonical IDs are retained and
incident Links prevent deletion; no cascade is performed.`},
		{depAddCmd, `Use bd dep add issue blocker for a blocking
Dependency between two live Issues. Memory endpoints, remote routing and bulk
dependency flags are unavailable in this preview.`},
		{linkCmd, `Without --link-type, bd link SOURCE TARGET creates the ordinary
blocking Dependency between two live Issues. For a Memory or Issue endpoint,
choose an installed informational Type, for example on a fresh workspace:
  bd link policy work --link-type types/example-cites
Use bd types to see Link Types installed in this workspace. --link-type
accepts types/NAME or a full local Type URL. An optional --id
selects links/PATH; --properties supplies informational Link properties.
Memory-owned Links accept the current source by default, or use
--if-source-revision TOKEN to reject a stale source. --unconditional-source
explicitly selects the default. The blocking Type types/preview-blocks-v1
requires Issue endpoints and, unlike informational Types, one of
--if-source-revision TOKEN or --unconditional-source.`},
		{closeCmd, `Close one live Issue by ID or beads/ID. Batch, force and
remote-routing forms are unavailable in this graph preview.`},
		{reopenCmd, `Reopen one closed Issue by ID or beads/ID, optionally
with --reason. Batch and remote-routing forms are unavailable.`},
		{readyCmd, `Show current ready Issues with no graph-specific filters.
This graph preview refuses positive BEADS_MAX_ROWS instead of truncating.`},
		{listCmd, `Without Issue filters, bd list reads one bounded snapshot of current Beads
of all installed Bead Types and lists every Memory and every Issue the
ordinary bd list would show, newest recorded change first. Closed and pinned
Issues are hidden unless --all is given; --all also removes the row limit.
Each human row shows the local ID and kind; an Issue row also shows its
status and priority. Use --bead-type types/NAME to narrow by Bead Type.
--limit returns a prefix with hasMore, not a continuation cursor. A positive
BEADS_MAX_ROWS refuses a page of more Beads than that.
Issue-specific filters (--status, --type, --title, --title-contains,
--priority, --priority-min, --priority-max, --assignee, --no-assignee,
--label, --label-any, --exclude-label, --pinned, --no-pinned, --due-before,
--due-after, --overdue, --sort, --reverse) or a matching configured
directory label switch to the existing Issue query: Issues only, closed and
pinned Issues omitted unless --all or a filter selects them, quoted rows
with status and priority, and a line under the header saying Memories are
not listed. A typed filter cannot be combined with a non-Issue --bead-type;
a configured directory label alone does not refuse one, it is simply not
applied to Memories. Tree output and legacy --json are unavailable.`},
		{blockedCmd, `Show current dependency-blocked Issues with canonical
blocker IDs. Filters and positive BEADS_MAX_ROWS are unavailable.`},
		{graphCmd, `Use bd graph ID --view generic to traverse the current
local graph. Control direction, depth, node and Link bounds with --direction,
--depth, --max-nodes and --max-links. Legacy visualization modes are unavailable.`},
		{statusCmd, `Use bd status --graph to inspect this workspace's supported
graph capabilities and limits. Ordinary issue-statistics mode is unavailable.`},
		{typesCmd, `Use bd types to list the Bead and Link Types actually installed
in this workspace, grouped by category. The displayed types/NAME IDs are
accepted by --bead-type and --link-type. Add --details to show each complete
persisted Type descriptor. --json returns the full descriptors as structured
data. Legacy Issue classifications such as task and bug belong to --type;
--sections is unavailable in graph preview workspaces.`},
	} {
		entry.cmd.Long += "\n\nGraph preview workspaces:\n" + entry.text
	}
}
