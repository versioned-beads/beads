# Graph CLI Specification (Draft)

For setup and task-oriented commands, start with the
[graph CLI guide](/reference/graph-cli). This page specifies the intended
graph CLI when it is ready for main. Its implementation ledger separately
records what the current integration build supports and which release is
targeted for unfinished work. The blog post is the publication narrative;
this specification and the CLI guide continue to change with the product.

The currently implemented subset supports a bounded Memory/Issue workflow in
a **fresh, explicitly selected graph workspace**. Existing ordinary Issue
workspaces continue using their existing commands and storage. This draft does
not promise migration or that every target operation already runs.

## Target graph CLI contract

This is the proposed complete graph-mode CLI contract, including commands
that are not implemented yet. The [graph CLI guide](/reference/graph-cli)
remains the task-oriented walkthrough. The implementation ledger below is a
snapshot of `versioned-beads/beads:integration` at
`356275a13290064fe903ace31984c14d1b9f7ad4`; it is not a claim that
every target command works. Update that commit and the ledger whenever the
integration source changes, and reconcile this specification and the guide
with each admitted behavior change. A candidate PR does not become current
behavior until it lands and the ledger is updated.

This contract covers all graph-affected Bead, Link, History, discovery,
agent-setup and serving entry points. Ordinary-project commands keep their
ordinary contracts. Type descriptor design, installation, removal, update and
migration belong to the separate Type design and BDP specification; this CLI
contract covers only reading installed Types and choosing one for a Bead or
Link. Administration, sync, import/export, backup and repair are not implicitly
admitted by graph mode. A recognized command or flag excluded from graph
admission returns `capability_unavailable` without opening the ordinary Issue
store; an unknown spelling may be rejected by the parser first.

### Common syntax, identity and write rules

| Input | Contract |
| --- | --- |
| Workspace | `bd init --graph-mode link --scope-url URL` creates a **new** graph workspace. An existing ordinary, incomplete or incompatible graph workspace is never upgraded by init. Subsequent graph commands bind to that workspace's stored Scope and format; moving/copying it does not transfer authority. |
| Resource selector | A command that accepts both Beads and Links requires `beads/PATH` or `links/PATH`, or the exact local Scope URL, for either kind. A Bead-only command accepts bare `ID` as shorthand for `beads/ID`; a Link-only command accepts bare `ID` as shorthand for `links/ID`. The kind comes from the command, never from probing both namespaces. No foreign URL or alias is silently resolved. The current build still accepts a bare Bead ID on some mixed-resource commands and requires `links/PATH` on Link-only operations; those parser changes remain NYI. |
| Creation ID | `--id` is optional on `bd create`, `bd remember` and `bd link`. Omission allocates a fresh canonical ID. `bd create` and `bd link` use an explicit ID exactly or refuse if it was ever allocated; deletion does not release it. `bd remember --id ID` creates a Memory at an unused ID or updates the existing Memory there, while `--create-only` requests duplicate refusal. A Link's `--id` takes the Link-only bare-ID shorthand; this is NYI in the current build. |
| Type selector | `--bead-type` and `--link-type` accept an installed `types/NAME` or its exact local Type URL. An uninstalled, wrong-kind or endpoint-incompatible Type fails before a write. `--type` on an Issue is its Issue classification, not a Bead Type. |
| Current and retained state | A bare read selects the current record. `--version TOKEN` selects one retained state; `bd versions` discovers tokens in store-local newest-first order. A complete graph Resource record exposes one opaque `revision` token, used for current-state write guards. The versions list calls the same kind of opaque exact-read address `version`; that name and `--version` remain for retained reads. Neither token encodes chronology. The current build still emits a redundant `version` field on full Resource records; removing it is an approved target change, not yet implemented. |
| Guarded write | `--if-revision TOKEN` compares the current revision of the Resource being changed. `--if-source-revision TOKEN` separately compares the owning **source Bead** when a write changes one of its outgoing owned Links. The installed preview Issue Type owns blocking Dependencies, and the installed Memory Type owns its outgoing informational Links; an informational Link from an Issue is not owned by that Issue Type. Ownership comes from the installed Type descriptor, not the Link's name or a Memory-only rule. A stale check rejects the whole write, including a would-be no-op. |
| Unconditional write | `bd remember --id ID` creates or updates by default; `--create-only` reverses that default and refuses an allocated ID. The older `bd remember --update ID` spelling remains supported for callers that explicitly select an existing Memory; it does not create. An existing-ID Memory update defaults to accepting the current state, as does a Memory-owned informational Link **source**. `--if-revision` opts into a stale-write refusal. Property/scalar `update`, `unlink` and applying a deletion currently require either `--if-revision` or explicit `--unconditional`; blocking Dependency unlink also requires an Issue-source choice. `close`, `reopen` and standalone `update --claim` use native Issue policy without that guard choice. Do not combine guarded and unconditional spelling for one resource. A changed unconditional Memory/source write reports the actual replaced version and attribution. Whether to extend unconditional defaults to all Bead operations is an open contract decision. |
| No-op and uncertainty | A semantically identical accepted edit records no new version, does not renew a claim and does not attribute an overwrite. An `outcome_unknown` result is never safe to replay blindly: inspect the canonical resource and versions before retrying. |
| Read-only | `--readonly` and a frozen workspace reject every mutation before it writes. They do not relax identity, validation or read bounds. `--force` confirms only the operation named by a command; it never bypasses a guard or cascades a deletion. |

`bd remember` shares the Memory creation writer with `bd create`, but is not
just an alias for it. Its explicit-ID convenience must also update an existing
Memory, preserve omitted title/body fields, apply an optional observed-revision
guard, and refuse a deleted or non-Memory identity. That update uses the Memory
patch writer; the create writer continues to reject duplicate IDs.

The common graph controls are `--graph-mode` (assert `link`),
`--directory` (workspace selection), `--actor` (attribution),
`--readonly`, `--quiet`, `--no-color` and `--json` where that command has a
structured result. `--db`, `BEADS_DB`, `BD_DB`, `BEADS_DIR` and backend
overrides may affect **selection or refusal**, never redirect an admitted graph
command into an unrelated store. Command-specific refusals of a common flag
are explicit below. Flag omission differs from an explicitly supplied empty
value; accepted empty values are described in the command-family sections
below.
Except for documented repeatable label filters, repeating a scalar selector is
invalid. A flag not listed for an entry point is unavailable in graph mode.

### Relationship to BDP

The graph CLI and BDP are two ways to work with the same Scope, Beads, Links
and installed Types. For capabilities both expose, they should agree on
canonical identity, Type and property meaning, opaque revision equality,
accepted changes versus no-ops, and refusal of invalid or stale mutations.
CLI spelling and result presentation need not mirror HTTP requests. A local
CLI operation does not, by itself, advertise a BDP HTTP capability: the
current listener serves the Read profile only.

| CLI surface | Corresponding BDP concept | Boundary to keep explicit |
| --- | --- | --- |
| `types`, `show`, `recall`, `list`, `memories`, `links` | Type and Resource reads, Bead/Link collections, and a Bead's incident-Link view | `recall` extracts a Memory body for a terminal; CLI listing is bounded and may apply Issue-specific filters, while BDP collections use cursors and generic selection. |
| `create`, `remember`, `link` | Create Bead or create Link | BDP mutation targets belong to a write-capable profile and are not served by the current Read listener. CLI convenience fields and Issue defaults must resolve to the same resulting Resource state when a corresponding target exists. `remember --id` may route to an update of an existing Memory; BDP create itself still refuses a duplicate. |
| `update`, existing-ID `remember`, `close`, `reopen`, `update --claim`, `defer`, `undefer`, `unclaim` | Change Bead or Link properties | The Issue lifecycle commands carry native eligibility, claim-lease and History rules. Their correspondence to a generic BDP mutation is a design and conformance question, not a promise that one HTTP request already reproduces them. |
| `delete`, `forget`, `unlink` | Delete Bead or delete Link | The CLI's explicit-apply affordance and incident-Link refusal must not silently become a protocol cascade. HTTP deletion is not served yet. |
| `versions`, graph `history`, `show --version` | Retained Resource state | The BDP v0 draft defines `view=versions` pages and exact historical reads for History-capable services, but the current Beads Read listener does not serve that surface. BDP rows carry opaque `revision`, lineage and retained-body status, with cursor pagination; the CLI's `local_revision` is not a BDP wire member. BDP Events and changefeeds remain separate from retained-version reads. |
| `compare` | Compare two retained Resource states | This is a local CLI convenience over two exact states, not a separate BDP operation. |
| `graph`, `ready`, `blocked`, `dep`, `status`, `init`, `setup`, `serve` | CLI composition, Issue workflow, or workspace administration | These are not additional generic BDP Resource verbs. `dep add` creates a Link, but readiness and administration do not acquire protocol endpoints by having CLI commands. |

As each shared capability is implemented, test both projections against the
same Resource behavior and record any intentional difference in this table.
Do not claim BDP Read+Update or Transactional conformance from CLI coverage;
those profiles have separate transport and guarantee requirements. The open
unconditional-write policy decision above also needs to be reconciled across
both projections before either is called final. The approved CLI naming keeps
`revision` on full Resource records, `version` on retained-list rows and the
`--version` exact-read flag; BDP History rows retain `revision`.

### Implementation ledger and explicit-flag inventory

This table describes the admitted graph-mode surface at the pinned integration
commit above. The target contract also includes the not-yet-admitted additions
below. Command-specific behavior is defined in the following sections; the
common controls are listed above. Hidden `--resource-type` is a compatibility
spelling for `--link-type`, never an additional Type field; supplying both is
invalid. An implementation marked NYI must refuse without falling back to
ordinary storage.

| Entry point and arguments | Additional explicit flags and aliases | Current integration behavior |
| --- | --- | --- |
| `init` | `--scope-url`, `--prefix`, `--server`, `--external`, `--server-host`, `--server-port`, `--server-user`, `--server-socket`, `--server-tls`, `--database`, `--skip-hooks`, `--skip-agents`, `--non-interactive` | New graph workspace only / implemented. |
| `status` | `--graph` | Report exact capability and limits / implemented; `--graph` required in a graph workspace. |
| `types` | `--details` | Read installed Bead/Link Type IDs, or full stored descriptors / implemented. Type mutation commands are outside this contract. |
| `remember [BODY]` | `--id`, `--title`, `--body-file`, `--stdin`, `--update`, `--if-revision`, `--unconditional` | Create or explicitly update one Memory / implemented. Exactly one body source when body is supplied; omitted fields survive update. |
| `memories [SEARCH]` | `--all`, `--details`, `--format table\|records-json` | Bounded current Memory discovery / implemented; legacy `--json` refuses. |
| `recall BEAD` | `--version` | Exact body bytes, current or retained / implemented; `--json` refuses and `--quiet` does not suppress body bytes. |
| `create [TITLE]` | `--bead-type`, `--id`, `--title`, `--description`/`--body`/`--message`; Issue-only `--type`, `--priority`, `--labels`/`--label`, `--design`, `--acceptance`, `--assignee`, `--estimate`, `--external-ref`, `--spec-id`, `--notes`, `--due` | Create one Issue by default or Memory by Type / implemented. No file/stdin body route here. |
| `show RESOURCE` | `--version` | Current or retained complete record / implemented. |
| `versions RESOURCE` | no additional flags | List one Resource's retained states newest first. The rows supply tokens for exact reads and comparison. |
| `history RESOURCE` | no additional flags | In a graph workspace, an alias for `versions RESOURCE` with the same rows. In an ordinary workspace, `bd history` has a different Dolt-commit contract; do not assume the graph alias there. |
| `compare RESOURCE` | `--from`, `--to` | Two explicit retained states of the same Resource / implemented. Operand order determines direction. |
| `update MEMORY` | `--properties` or `--patch`, `--if-revision`, `--unconditional` | Whole-property replacement or ordered patch / implemented. Body/title-only convenience uses `remember --update`. |
| `update ISSUE` | `--title`, `--description`/`--body`/`--message`, `--design`, `--acceptance`, `--priority`, `--assignee`, `--append-notes`, `--estimate`, `--external-ref`, `--spec-id`, `--due`, `--if-revision`, `--unconditional`; standalone `--claim` | Listed scalar edits and append / implemented. `--claim` is mutually exclusive with every edit/guard. Label edits and note replacement are specified below, NYI. |
| `update LINK` | `--properties` or `--patch`, `--if-revision`, `--unconditional`, `--if-source-revision`, `--unconditional-source` | Informational Link properties only / implemented. Endpoints and Type are immutable. |
| `link SOURCE TARGET` | `--link-type`/`--resource-type`, `--id`, `--properties`, `--if-source-revision`, `--unconditional-source`; blocking alias `--type` | One typed Link, or an Issue-only blocking Dependency with ordinary default Type / implemented. `--unconditional-source` explicitly accepts the current owning source Bead instead of checking a supplied `--if-source-revision`; for a Memory-owned informational Link, that source acceptance is already the default. Remote endpoints, bulk and bypass flags refuse. |
| `dep add SOURCE TARGET` | `--type` | One local blocking Dependency between live Issues / implemented. No bulk form; Link Type and source-guard flags belong to `bd link` and are refused here. |
| `links BEAD` | `--direction in\|out\|both`, `--link-type`/`--resource-type` | Bounded current incident Links / implemented, no cursor. |
| `unlink LINK` or `unlink SOURCE TARGET` | `--link-type`/`--resource-type`, `--if-revision`, `--unconditional`, `--if-source-revision`, `--unconditional-source` | Remove an identified informational Link or blocking Dependency; pair form must identify exactly one informational Link / implemented. |
| `delete BEAD`, `forget BEAD` | `--if-revision`, `--unconditional`; `delete` also `--force` | Preview then explicit apply, or direct apply, for an unreferenced Memory / implemented. Issue deletion target is specified below, NYI. |
| `close ISSUE` | `--reason`/`--resolution`/`--message`/`--comment` | Native close of one Issue / implemented. No force or batch form. |
| `reopen ISSUE` | `--reason` | Native reopen of one Issue / implemented. |
| `ready`, `blocked` | no additional flags | Native Issue readiness and blocker views / implemented; no query filters. |
| `list` | `--flat`, `--format records-json`, `--bead-type`, `--all`, `--limit`; Issue query: `--status`/`--state`, `--type`, `--title`, `--title-contains`, `--priority`, `--priority-min`, `--priority-max`, `--label`, `--label-any`, `--exclude-label`, `--pinned`, `--no-pinned`, `--sort`, `--reverse`, `--assignee`, `--no-assignee`, `--due-before`, `--due-after`, `--overdue` | Bounded all-Bead list unless an Issue filter/directory label selects native Issue-only query / implemented. Tree and legacy `--json` refuse. |
| `graph BEAD` | `--view generic`, `--direction in\|out\|both`, `--depth`, `--max-nodes`, `--max-links` | Bounded current local summary traversal / implemented. |
| `serve` | `--readonly`, `--addr`, `--allow-non-loopback`, `--auth-token-file`, `--insecure-no-auth`, `--allowed-host` | BDP Read listener on ordinary shared-server Dolt / implemented. Embedded serving and HTTP mutation refuse. |
| `setup claude` | `--project`, `--check`, `--remove` | Project-local Stop reminder registration/check/removal / implemented; `--json` refuses. |
| `claude-hook stop` | no additional flags | Hook JSON stdin/stdout protocol, no storage open / implemented; no CLI `--json`. |

The following additions belong to the final target contract but are **NYI at
the pinned integration commit**. They do not enlarge its admission inventory.
Each argument and flag combination must be implemented and tested before its
row can move into the current inventory. Release targets are planning status,
not permission to advertise an NYI operation.

| Surface | Target release | In pinned integration build | Candidate work |
| --- | --- | --- | --- |
| Existing command inventory above | Already in integration | Yes, within each row's stated bounds | Preserve while integrating new work. |
| Existing-ID `remember` default upsert and `--create-only` | Preview 2 candidate | No; `--id` creates only and `--update` selects an existing Memory | Route the CLI convenience to the existing create or Memory patch writer without changing BDP's distinct create/update operations; test both engines and keep the current-build help truthful until it lands. |
| Issue defer/undefer, unclaim and deletion | Preview 2 candidate | No | Draft #105, #106 and #108; their combined source still needs qualification. |
| Full ordinary Issue `close`, `reopen`, `ready` and `blocked` behavior | **Preview 2 mandatory; no dependent flag is NYI in the release target** | No; the current graph commands have the narrower shapes in the inventory above | Implement and test the complete ordinary command contracts, including batch and interactive behavior, molecule/ephemeral and parent controls, metadata queries, output choices and atomic ready-claim. A flag whose underlying graph feature is absent is unfinished release work, not a permanent graph exception. The [Issue lifecycle binding](#issue-lifecycle-binding) enumerates the flags and effects. |
| Graph close policy, batch, next-work, observed-revision guard and interactive fallback | Slices of the mandatory ordinary close parity | No; pinned integration still has the narrower graph close route | Combined fork draft [#74](https://github.com/donnabox/beads/pull/74) includes checked `--force`/`--session`, positional and file reasons, mixed batch results, `--suggest-next`, atomic `--claim-next`, and single-Issue `--if-revision`. Its guard checks the complete graph Issue revision before the native close writer, including an already-closed retry; it cannot combine with a batch or next-work flag. Successor draft [#75](https://github.com/donnabox/beads/pull/75) carries the ordinary interactive last-touched Issue fallback through create/show/update/close and ready claim while keeping noninteractive no-ID writes refused by default. Evaluable gates, `--continue` and molecule effects remain required before the full close row is complete. These drafts await combined-source qualification and review. |
| Explicit mixed-resource selector disambiguation | Decision for the final CLI; release not assigned | No; some mixed commands accept bare Bead IDs | Parser and help changes required after contract review. |
| Arbitrary installed Bead authoring and Type lifecycle | After the Type design is decided; release not assigned | No | Separate Type workstream owns descriptors and lifecycle. |
| Open Bead/Link metadata | Preview 2 mandatory | No | Implement common metadata for both admitted Bead kinds and informational Links. The BDP contract decision is one atomic Resource update across properties and metadata, with `{}` as the empty read value, including for older retained states without stored metadata. The normative spec, schemas and conformance work remain pending; Type lifecycle stays separate. |
| Issue and Memory deletion through `bd delete` | Preview 2 mandatory | Memory only, when unreferenced | Draft #108 adds the Issue route; qualify the combined generic Bead lifecycle and retained reads on both engines. `forget` remains a Memory compatibility spelling. |
| Issue notes replacement/clear | Preview 2 candidate, after the core lifecycle | No | Separate stacked fork PR #62 implements native overwrite/clear behavior; qualify and review it on the combined source. |
| Issue label mutation | Preview 2 opportunistic, after mandatory work | No | Admit only with isolated transactional staging and installed two-engine proof; otherwise cut at the release decision. |

When integration advances, update the pinned commit, the rows above and any
current-build statements throughout this document in the same change. A draft
branch may demonstrate a candidate without changing the integration column.

| Entry point | Proposed additions | Rule |
| --- | --- | --- |
| `create [TITLE]`, `remember [BODY]` | `--properties JSON` for generic `create`; `--metadata JSON` for either creation route | A generic property document requires `--bead-type` and excludes Issue/Memory convenience fields. Metadata is a separate JSON object and may accompany admitted Issue or Memory creation. On existing-ID `remember`, it is a metadata merge under the same Memory guard/default as the body/title edit. |
| `link SOURCE TARGET` | `--metadata JSON` | Sets the initial open metadata object on one typed informational Link; it is unavailable for a blocking Dependency. |
| `update RESOURCE` | `--metadata JSON`, repeatable `--set-metadata KEY=VALUE`, repeatable `--unset-metadata KEY` | Match ordinary `bd update`: `--metadata` merges top-level keys inside the write transaction and cannot combine with set/unset; set and unset may combine, with unset applied last. Omission preserves metadata. Metadata can accompany an admitted property/scalar edit in the same atomic write and uses its Resource and applicable source guard. |
| `update ISSUE` | `--add-label X`, `--remove-label X`, `--set-labels X,Y`; `--notes TEXT`, `--clear-notes`, `--force` | Label flags may combine as in ordinary `bd update`: replace, then add, then remove, so removal wins. Notes use the ordinary overwrite fence and the existing graph Issue revision-guard choice below. |
| `delete BEAD` | existing `--force`, `--if-revision`, `--unconditional` | The same preview/apply spelling works for both admitted Bead kinds, Memory and Issue, with incident-Link refusal and retained prior states. The current integration branch admits only Memory; draft PR #108 adds Issue. |
| `defer ISSUE...`, `undefer ISSUE...` | `defer`: `--until`, `--reason`; optional graph `--if-revision TOKEN` or `--unconditional` for one Issue | Target: ordinary `bd` defer/undefer behavior, including dated wake and multiple IDs. The current integration branch does not offer graph defer/undefer; draft PR #105 implements the target behavior but has not landed. |
| `unclaim ISSUE...` | `--reason`, `--force` or `--if-assignee HOLDER` | Target: ordinary `bd` release behavior, including the ownership and conditional-release rules. The current integration branch does not offer graph unclaim; draft PR #106 implements the target behavior but has not landed. |
| `close [ISSUE...]`, alias `done` | `--reason` (one shared or positionally repeated), `--reason-file`, `--force`, `--session`, `--suggest-next`, `--claim-next`, `--continue`, `--no-auto`; ordinary conditional `--if-revision` behavior | Match ordinary close eligibility, batch result/partial-failure behavior, interactive last-touched fallback, pinned/gate force rules, session attribution, next-work discovery and claim, and molecule advancement/auto-close. Honor the ordinary single-target restrictions on workflow and conditional flags. |
| `reopen ISSUE...` | `--reason` | Require at least one ID, accept multiple IDs, clear the closed time and emit the ordinary Reopened event on accepted transitions; preserve native no-op behavior. |
| `ready` | `--assignee`, `--claim`, `--exclude-label`, `--exclude-type`, `--explain`, `--gated`, `--has-metadata-key`, `--include-deferred`, `--include-ephemeral`, `--label`, `--label-any`, `--limit`, `--metadata-field`, `--mol`, `--mol-type`, `--offset`, `--parent`, `--plain`, `--pretty`, `--priority`, `--sort`, `--type`, `--unassigned` | Match ordinary blocker-aware filtering, ordering, presentation and atomic first-match claim. Preserve the ordinary proxied-server restriction on `--offset`; implement its supporting graph execution path rather than silently accepting an ineffective flag. |
| `blocked` | `--parent` | Match ordinary dependency-blocked results and descendant filtering. |

The implemented defaults are part of the contract, not unspecified CLI
convenience: `link` without a Type requests a blocking Issue Dependency;
`links` uses `--direction both`; `graph` requires `--view generic` and
defaults to direction `both`, depth 1, 100 nodes and 200 Links. `list` uses
the documented terminal-sensitive row limit, while `--all` or `--limit=0`
removes that limit; an explicit `--limit` wins. `memories` without SEARCH is
an inventory, not a read of an arbitrary Memory. On `create`, `--type`
classifies an Issue and must not be interpreted as `--bead-type`.
The preview adapter stores ordinary Issue classifications such as `task` and
`bug` in the `issue_type` property of one installed Issue Bead Type. BDP's
generic model treats distinct Bead Types as nominal Types, so a BDP collection
filter on `type` cannot select one ordinary Issue classification in this
preview. That adapter mapping preserves the existing CLI; it does not settle
Trish's Type-lifecycle design or redefine BDP nominal typing.
`--status` and `--state` are alternative Issue filters and cannot be combined.
An Issue filter selects the Issue-only `list` route even when its value is
empty; it cannot be combined with a non-Issue `--bead-type`.

### Results, errors and repeatability

The target single-target success contract is one complete result on stdout,
with diagnostics on stderr. Multi-Issue lifecycle commands process targets in
input order, as ordinary `bd` does: successful targets remain committed and
appear on stdout even if a later target fails. Per-target failures appear on
stderr and the command exits nonzero. A command-wide syntax or admission
failure rejects before processing any target. Most `--json` success results use
`{"schemaVersion":1,"preview":true,"result":<typed result>}`; the typed
result shape for a command is the shape shown by that command's examples and
tests below. `memories` and `list` use `--format records-json` for their
structured result in the same envelope and refuse legacy `--json`. `recall`
streams exactly one Memory body, with no added newline or envelope;
`--quiet` cannot suppress it. `setup claude`, `claude-hook stop` and
`serve` use their own human, hook or server protocol, not a CLI JSON
envelope. Current `serve --json` is accepted but does not produce a CLI JSON
envelope; the target contract refuses that combination so scripts do not
mistake a running server for a JSON result. `--quiet` suppresses optional
human success prose elsewhere; it never hides an error. `--no-color` changes
presentation only.

| Exit | Error category | Required behavior |
| --- | --- | --- |
| 0 | Success or accepted semantic no-op | Emit the selected complete success form; an identical write does not create a new version. |
| 1 | Partial multi-target Issue command failure | Preserve and report successful targets on stdout, report failed targets on stderr, and let callers inspect per-target results before retrying. This does not roll back earlier successful targets. |
| 2 | Invalid syntax or selector | Reject before a write; examples include conflicting aliases, malformed JSON, unsupported flag combinations and invalid bounds. |
| 3 | Missing, gone or unknown retained version | Distinguish a never-present current ID, a removed current ID and an unavailable version; never substitute the current state for an old address. |
| 4 | Revision/identity/constraint conflict | No partial write to the affected target. An earlier target in a multi-target Issue command may already have succeeded. Include enough canonical IDs to act on an ambiguous pair unlink or target incident-Link deletion refusal. The current linked-Memory deletion refusal is `deletion_policy_unresolved` at exit 5 until the proposed stable constraint error is implemented. |
| 5 | Unavailable capability, workspace/authority refusal or storage failure | Never fall through to an ordinary workspace or claim an unsupported feature ran. |
| 6 | Outcome unknown | The client must inspect current state and retained versions before deciding whether to retry; the command must not claim failure or success of a write whose commit result is unknown. |

Machine-readable errors use
`{"code":"...","message":"...","retryable":false}` on stderr;
human errors use `code: message` there. A command-wide admission or validation
refusal prints no success envelope; a failed target prints no success record
for that target, while successful targets in a batch remain visible. An
ambiguous-Link error must report sorted candidate canonical IDs. A future
retryable category may change only with a separately reviewed contract; the
current graph errors do not ask a caller to blindly retry. Scalar selectors
and scalar edit flags are single-valued even if Cobra accepts repeated syntax:
repetition rejects rather than silently taking the last occurrence. The
repeatable `list --label`, `--label-any` and `--exclude-label` filters are
the exceptions and combine according to their documented Issue query rules.
This scalar-repeat rule is a **target conformance requirement** beyond the
current parser's per-command checks, not a claim of existing enforcement.

This is a graph CLI result contract, not a description of ordinary `bd`'s
existing output. Ordinary commands have command-specific JSON shapes and
usually report errors with exit 1; graph commands deliberately use the
preview envelope and typed exit categories above so scripts can distinguish
missing state, conflicts, unavailable capabilities and uncertain commits.
The current graph build already uses these result/error forms for admitted
commands. Refusing `serve --json` and rejecting repeated scalar flags are
still target-only changes, called out above; they must not be described as
current behavior until implemented and tested.

### Issue lifecycle binding

Graph Issues use the ordinary Issue writer's policy for status categories,
readiness, blocked prerequisites and claims, with the graph-specific CLI
surface fixed here. This is an explicit normative dependency on the same
release's [close](/cli-reference/close),
[reopen](/cli-reference/reopen), [ready](/cli-reference/ready) and
[update/claim](/cli-reference/update) contracts, not permission to dispatch a
graph command through the ordinary workspace. A change to those ordinary
contracts must be reviewed against this graph contract and its conformance
tests. Graph Memories and informational Links have no Issue status, readiness,
claim or close operation.

| Entry point | State and transaction contract |
| --- | --- |
| `create` Issue | Starts open, with the accepted initial fields and no claim lease. Initial assignee is data, not an atomic claim. |
| `update ISSUE --claim` | One native atomic claim for the current actor; true-only and no edit/guard flags. Eligibility follows the configured native active-status and pool-alias rules. A successful change retains the complete Issue state and native claim stamp together. Repeating an eligible same-actor claim is a no-op and never renews its five-minute nonrenewing lease. A different actor cannot take an active claim by merely spelling `--unconditional` or `--force`. |
| `close [ISSUE...]` | Native checked close and ordinary command behavior for zero, one or multiple IDs; the zero-ID last-touched fallback applies only under the ordinary interactive rule. Accepted close writes retain one complete graph projection with the native stamp. An already-closed Issue is a no-op. `--force` follows native pinned/gate policy; it does not bypass an explicit revision conflict. Post-close suggestion, atomic claim-next and molecule continuation have their ordinary lifecycle effects and restrictions. A refusal leaves that target's status, Links and retained state unchanged. |
| `reopen ISSUE...` | One or more IDs, native transition from a done-category status to open, `closed_at` clearing and Reopened event. A non-done Issue is a no-op after validation; each accepted change retains the complete graph projection with the native stamp. |
| `ready`, `blocked` | Native blocker-aware Issue views with all ordinary query and presentation controls. `ready --claim` atomically claims the first matching Issue and records the native claim/projection once. Its `--json` result is an array of complete graph Issue records with one element on success and zero when the filtered front is empty, preserving ordinary ready's array shape without inventing an Issue mutation wrapper. An empty claim itself records no version or lease; the ordinary lazy defer-wake before ready selection may separately version an expired deferred Issue. Apart from that wake, a ready read changes no state; blocked reads change no state. `blocked --parent` filters descendants while preserving canonical blocker IDs. |

The current integration build has narrower graph command shapes: one explicit
ID for `close` or `reopen`, no `ready` query flags and no `blocked --parent`.
That inventory describes the build, not the Preview 2 release target. The
release target is the complete ordinary command contract for these four
commands, including dependent molecule, ephemeral, parent, metadata and
post-close behavior. No ordinary flag or lifecycle effect in this contract is
designated NYI for Preview 2. `update --claim` remains a standalone graph
entry point; `ready --claim` additionally follows the ordinary atomic
first-match behavior. The ordinary command's own restrictions still apply:
for example, `reopen` requires an ID, `close` permits a zero-ID fallback only
where ordinary interactive policy permits it, and `ready --offset` requires
the proxied-server path. If a supporting graph model or execution path is
missing, that is mandatory implementation and qualification work before the
release can claim parity.

The native close/reopen/claim writer owns the atomic History stamp. The graph
layer retains the resulting complete Issue projection in that same
transaction; it must not mint a second stamp, substitute a fabricated time or
take a separate application lock. A native no-op must not acquire a new graph
version. Changing writes update the store-wide `writer_token` fence inside
that transaction; the fence is not a second application lock. `defer` and
`undefer` are specified below as NYI; no current Issue
read or elapsed clock silently implements those target transitions.

### Target operations that remain NYI

These rows complete the intended non-Type CLI behavior for review; they do not
extend this build's admission. Every proposed operation must gain an
implementation, help text, two-engine installed tests and exact-source CI
before its status changes. Existing incident-Link deletion refusal is retained:
neither `--force` nor a future convenience command may silently cascade.

| Operation | Complete proposed contract | Current status |
| --- | --- | --- |
| Arbitrary installed Bead authoring | `bd create --bead-type types/NAME --properties JSON [--id ID]` creates one Bead for any installed Bead Type that the current workspace can validate; the JSON value is an object and must satisfy that Type's descriptor before an atomic write. `--properties` is mutually exclusive with Issue/Memory convenience body, title and field flags, so a caller cannot create two competing documents. `bd update ID --properties JSON` replaces the whole validated document and `--patch JSON` applies an ordered patch; both require the existing-resource guard choice and preserve ID and Type. `bd show`, `bd list --bead-type`, `bd versions`, `bd compare` and `bd delete` work by canonical identity without assuming Issue fields. Type-specific lifecycle policy must be declared by the installed Type; the CLI never invents ready/close behavior for an arbitrary Bead. | NYI for authoring outside installed Issue/Memory writers. Current generic reads and nominal Type filtering cover only admitted installed records. Descriptor/installation semantics belong to Trish's Type design. |
| Open metadata | `bd create [TITLE] --metadata JSON`, `bd remember [BODY] --metadata JSON`, and `bd link SOURCE TARGET --link-type types/NAME --metadata JSON` accept an initial JSON object separate from Type-validated properties. On generic `create`, metadata may accompany `--bead-type types/NAME --properties JSON` but cannot make incomplete creation valid. The empty default is `{}`. On an existing Resource, `bd update RESOURCE --metadata JSON` merges top-level keys inside the mutation transaction, matching ordinary `bd update`; `bd remember --id ID --metadata JSON` uses its existing-ID upsert policy. Repeatable `--set-metadata KEY=VALUE` and `--unset-metadata KEY` perform typed-value key edits; they may combine, with unset applied after set. `--metadata` cannot combine with set/unset. Metadata may be cleared to `{}` by update/patch; clearing an already empty object is a no-op. Metadata and an admitted properties/scalar edit may be one atomic write, while property replacement and property patch remain mutually exclusive. Omission preserves metadata. The same Resource and applicable owning-source guards cover the entire edit; no mode changes ID, Type or Link endpoints. A semantic no-op retains the revision. Exact retained reads and comparison include accepted metadata; older retained states with no stored metadata read as `{}` without rewriting them. | **Preview 2 mandatory, NYI** in the pinned integration build. Cover Memory, Issue and informational Link records, not Type installation. The BDP atomic-update and empty-object decisions are settled; the matching normative spec/schema amendment and conformance fixtures remain pending before claiming CLI/BDP parity. |
| `delete BEAD` for Memory and Issue | `bd delete ID` previews either admitted Bead kind. `bd delete ID --force --if-revision TOKEN` or `bd delete ID --force --unconditional` applies to one Bead, atomically checking its current revision and incident Links. Any incident Link refuses with sorted canonical Link IDs; the caller explicitly unlinks and retries with a fresh revision. A successful deletion removes current state, reserves the ID and retains prior snapshots; current reads report gone, exact prior-version reads remain addressable. No deletion version, fabricated timestamp, automatic dependency unlink or cascade is promised. `forget` remains Memory-only. This does not invent deletion for a future Type until its descriptor and writer exist. | **Preview 2 mandatory.** Unreferenced Memory preview/apply works in pinned integration; Issue deletion is NYI there. Draft PR #108 implements the Issue route but has not landed or completed combined-source qualification. |
| Linked Memory deletion | Same explicit unlink-before-delete rule as for an Issue. A linked Memory is never deleted by `--force`; its current identity and snapshots remain after refusal. Once unlinked, ordinary unreferenced Memory deletion applies. | The refusal is implemented on integration with `deletion_policy_unresolved`. Draft PR #108 changes the typed refusal to `constraint_violation` while retaining the Memory, its ID and snapshots; it has not landed or completed release qualification. |
| Issue labels after creation | `bd update ID --add-label X`, `--remove-label X`, and `--set-labels X,Y` change one Issue's label set under a resource revision choice. They may combine in one invocation: replace first, add second, remove last, so removal wins. Add-existing and remove-absent are no-ops; set replaces the set atomically, deduplicating normalized labels. A real label change advances the Issue RowVersion; a semantic no-op preserves it. Other fields and Links remain unchanged. Multi-Issue propagation is explicitly outside this CLI contract. | **Preview 2 opportunistic after the mandatory lifecycle/metadata work.** Initial `create --label/--labels` works; mutation is NYI. Admission requires staging that cannot publish unrelated pending Issue rows and installed two-engine proof. Otherwise cut it at the release decision. |
| Issue notes replacement/clear | `bd update ID --notes TEXT` sets nonempty notes. Replacing different, nonempty notes refuses unless `--force` is supplied; `--force` never bypasses a revision guard. `--notes=` always refuses because an accidental empty value could erase notes. `--clear-notes` deliberately clears without force. Omission preserves notes. The three notes modes are mutually exclusive. Like other graph Issue edits, either `--if-revision TOKEN` or explicit `--unconditional` is required. An identical replacement or clearing already-empty notes is a no-op; accepted changes retain prior snapshots and do not rewrite them. | **Preview 2 candidate after the mandatory core.** NYI in pinned integration; initial notes and append work there. Separate stacked fork PR #62 implements replacement/clear but is not merged or fully qualified. |
| Deferral | `bd defer ID...` accepts the ordinary optional `--until DATE` and `--reason TEXT` flags and one or more Issues. A date is parsed by the ordinary parser; without one the Issue remains deferred until explicitly restored. A reason appends to notes. `bd undefer ID...` restores a deferred Issue to open and clears `defer_until`; on a non-deferred Issue it clears a stale `defer_until` without changing status. An already non-deferred Issue without a date remains unchanged and is reported as such. Dated defers wake through the ordinary ready/claim behavior when due, with the native Issue writer owning the resulting History stamp and the complete graph projection retained in the same transaction. The graph CLI may offer an optional observed `--if-revision` on a single target, but normal unguarded spelling remains accepted; combining a guard with multiple targets refuses before writing. Batch behavior processes targets in input order with per-target results, matching ordinary `bd` rather than promising one all-or-nothing batch transaction. | NYI in the current graph integration. Draft PR #105 implements the target deferral behavior, including dates, reasons, multiple IDs and same-transaction wake projection; it has not landed or completed release qualification. |
| Claim release | `bd unclaim ID...` releases an assigned open or in-progress Issue, clearing assignee, started time and lease and returning it to open. Default release requires the current holder. `--force` bypasses holder authorization for an administrative release but never bypasses the native row/version race check. `--if-assignee HOLDER` instead performs the native conditional release and refuses a changed holder; it and `--force` are mutually exclusive. `--reason TEXT` records the ordinary explanatory comment. Multiple IDs use ordinary per-target processing and report failures rather than silently skipping them. Every accepted durable change must have its native History and complete graph projection; the graph adapter does not mint a second native stamp. | NYI in the current graph integration. Draft PR #106 implements holder, forced and conditional release, reason comments and multiple IDs; it has not landed or completed release qualification. |

This target intentionally does **not** assert that old graph workspaces migrate,
that a deletion has a citable deletion version, that `--force` bypasses Links,
or that any CLI operation silently supplies HTTP Write or HTTP History. Metadata
Type installation/migration, cross-Scope Link writing, batch graph
mutation, alias resolution, automatic recall and full compatibility with every
ordinary Issue command require separate contracts; they are not implied by
this table. The current implementation's capability report remains
authoritative until those contracts and code land.

Start in a new directory with no `.beads` directory:

```sh
git init
bd init --graph-mode link --scope-url https://example.org/team/ \
  --non-interactive
bd remember 'The release uses the integration branch.' \
  --id plan --title 'Release plan' --json
bd show beads/plan --json
# A separate invocation reopens the same stored Memory.
bd show https://example.org/team/beads/plan --json
bd status --graph --json
```

The Scope URL establishes local identity; initialization does not publish a
web server at that address. Every Bead is canonically under `beads/`; every
Link is under `links/`. The target contract requires the explicit namespace
for **both** kinds on commands such as `show`, `versions`, `compare` and
`update` that can select either kind. A Bead-only command such as `remember`
may accept bare `plan` as shorthand for `beads/plan`. The pinned integration
build still accepts a bare Bead ID in some mixed-resource commands, so the
stricter target syntax needs parser, help and example updates before admission.
Exact local Scope URLs remain accepted; aliases and foreign Scope URLs are
unavailable. Shorthand only changes CLI input, never stored identity or output.
Creation accepts an optional bare `--id ID` or canonical `--id beads/PATH`;
omitting it generates a random canonical ID. An explicit ID is used at its
canonical Bead path. `create` refuses duplicates; `remember` applies the
existing-Memory upsert rule unless `--create-only` is present. An allocated
identity cannot be reused for a different record.

For an ordinary external Dolt SQL server, add these options to `bd init`:

```sh
--server --external --server-host 127.0.0.1 --server-port 3306 --server-user root
```

Use the normal connection configuration appropriate to your server. Embedded
and server modes use the existing driver and standard schema initialization;
there is no manual schema seeding step. Graph initialization records and checks
workspace identity and refuses existing workspaces, mismatched bindings and
incomplete initialization. This preview does not adopt an existing Issue
database. Different-database provisioning on Dolt 2.1.8 must be serialized. A
workspace also records the format generation it was created with; see
[One bd version per workspace](#one-bd-version-per-workspace) for how that
decides which bd versions can open it.

### One bd version per workspace

A graph workspace records its format generation in
`.beads/graph-preview-format`. `link-preview-v5` is the original generation:
four installed Types, or six in a workspace created by a bd that already had the
two example Link Types, before `link-preview-v6` existed. `link-preview-v6` is
what this bd writes: the Memory, Issue, Dependency and Related Types plus the
two example Link Types. This bd opens both.

An older bd refuses a `link-preview-v6` workspace at its first command, before
it opens the database, with
`graph_mode marker and metadata disagree; automatic recovery is not supported`
(exit 5). An older bd that predates the example Types and opens a six-Type
`link-preview-v5` workspace gets as far as `bd list` or an inventory read and
stops with `unsupported Type installation`. Both messages mean the bd is older
than the workspace. Neither means the workspace is damaged, and nothing was
changed or lost. Upgrade bd. This preview does not support running an older bd
against a newer workspace, and it never rewrites a workspace's format.

A bd newer than this one that writes a later generation is refused here with
`graph_mode workspace format link-preview-vN is newer than this bd supports (link-preview-v6); upgrade bd; no database was opened`.

## Instructions for agents

Omit `--skip-agents` when initializing a graph workspace to install a managed
graph instruction block in `AGENTS.md` (or the configured agents filename).
The block incorporates Stephanie Jarmak's durable-memory guidance from
[integration PR43](https://github.com/versioned-beads/beads/pull/43), with
commands supported by this graph preview:

```sh
bd remember "Use UTC for timestamps" --id beads/time-policy --title "Timestamp policy"
bd recall beads/time-policy
bd memories timestamps --format records-json
```

The block also points to `bd status --graph` for the workspace's supported
capabilities.

Omit `--id` to allocate a distinct canonical ID for each new fact, or choose an
explicit ID as above. These commands demonstrate storage and retrieval across invocations; installing instructions does not
guarantee that an agent will decide to save a fact.

Graph initialization preserves surrounding user-authored text and an existing
`CLAUDE.md` import of `@AGENTS.md`. An existing minimal managed block is replaced
with graph instructions; pass `--skip-agents` to preserve that block unchanged.
By default, initialization also registers Stephanie's existing Claude Stop
reminder in project-local `.claude/settings.json` and adds an active import in
`CLAUDE.md` when needed. `--skip-hooks` omits the Stop registration and the
`CLAUDE.md` import; `--skip-agents` omits guidance, the import and the Stop
registration. Existing Claude settings, plugins and instructions are checked
for conflicts before a graph database is created; init refuses without
rewriting conflicting files, names the file to change, and suggests
`--skip-hooks`. If the Stop hook cannot be written after the database is
created (for example, an unwritable `.claude` directory), init prints a
warning and completes the workspace without it. Use `bd setup claude` to
install the Stop hook later in a workspace initialized with `--skip-hooks` or
after such a warning:

```sh
bd setup claude
bd setup claude --check
# Remove only the project Stop registration when no longer wanted:
bd setup claude --remove
```

This graph adapter adds only `bd claude-hook stop` to project-local
`.claude/settings.json`, preserving user settings values (including large integer
values), hook siblings, existing file permissions, and all
instructions (including `ProfileGraphPreview`). A changed settings file is
reformatted as sorted, two-space-indented JSON; original key order/whitespace
is not retained. A stale graph-profile hash refuses unchanged and requires
operator reconciliation of that managed block before setup. It creates or appends an active
`@AGENTS.md` import in `CLAUDE.md` (using the configured agents filename); an
existing active import and user text remain unchanged. Unsafe paths, unclosed
fences or stale managed Beads blocks refuse before settings/instructions are
changed. Removal retains the import and all instructions. `--project` is optional;
`--check` and `--remove` are mutually exclusive. It never installs `bd prime`
SessionStart/PreCompact hooks. Existing Beads plugins or prime hooks in project,
legacy-local or global settings cause installation/check to refuse unchanged;
reconcile those configurations deliberately first. Removal touches only the
managed Stop command in project `settings.json`; it does not remove legacy-local,
global or plugin registrations.

Global/stealth setup, other recipes, custom output/template flags and setup
`--json` are unsupported. The ordinary `bd prime` remains unavailable. The
Stop command accepts its native JSON stdin/stdout protocol without `--json`,
runs without opening storage, and follows Steph's transcript and reentrancy
rules: it reminds at a session's first Stop and again at a later Stop when
tools were used since the previous one, never while Claude Code reports
`stop_hook_active`. It does not write a Memory itself. After the reminder,
`bd remember "a fact"` stores the agent's chosen content and `bd recall ID`
reads it. Integration tests prove this delivery/execution sequence, not that
agents reliably choose what to remember.

Existing full or unknown managed profiles, malformed or duplicate managed
blocks, and symlink or nonregular targets refuse before database initialization.
Keep those files and pass `--skip-agents` to initialize without changing them.
If guidance publication to the agents file fails after database
initialization, the workspace remains incomplete and fenced; this preview
provides no repair command.

The ordinary minimal profile also includes Stephanie's exact durable-memory
contribution, which changes its managed content hash. Existing refresh policy
is unchanged; this addition does not independently rewrite instruction files.

On Memory creation, omitted `--title` uses the first nonempty body line with
whitespace collapsed, capped at 80 Unicode characters including an ellipsis.
The full body is preserved. A whitespace-only body yields an empty title. An
explicit creation title must remain nonempty; updates preserve omitted fields.

## Supported graph commands

| Command | Admitted scope and flags |
|---|---|
| `types [--details]` | List the Bead and Link Type IDs installed in this workspace. `--details` prints each complete persisted descriptor; `--json` returns the descriptors as structured data. An older four-Type workspace does not claim the two example Types. Legacy `--sections` is unavailable. |
| `remember BODY [--id ID] [--title TITLE]` | Without an ID, create a Memory at a fresh canonical ID. With an explicit ID, create if unused or update that live Memory by default, matching ordinary `bd remember --key` upsert behavior. Bare IDs resolve under `beads/`; one explicit `--body-file PATH` or `--stdin` replaces the positional body source. These sources are mutually exclusive; empty text is present content. Creation needs a body source and derives a title only when omitted. An existing-ID title-only update may omit the body and preserves it. A reserved deleted ID or an Issue at that ID refuses. |
| `remember --id ID --create-only` | Require a new Memory at this exact ID; any previously allocated ID refuses without changing it. Requires a body source and cannot combine with `--update` or update guards. This is the opt-in duplicate-refusal polarity; it is not a BDP create override. |
| `remember --update ID` | Existing-only compatibility spelling for changing supplied `--title` and/or one explicit body source; a missing Memory refuses. `remember --id ID --if-revision TOKEN` is also existing-only. Updates preserve omitted fields inside the transaction and default to unconditional acceptance when no guard is supplied. Explicit `--unconditional` remains accepted; `--create-only` cannot combine with either update spelling. |
| `memories [SEARCH]` | Complete bounded Memory title/body search summaries. Supports `--all`, `--details` and `--format table\|records-json`; legacy `--json` refuses. |
| `recall BEAD` | Stream one Memory's exact body bytes. Optional `--version TOKEN` selects a retained body. `--quiet` does not suppress content; `--json` refuses. |
| `update BEAD --properties JSON` | Replace a Memory's complete properties with exactly the `title` and `body` strings. Requires `--if-revision TOKEN` or `--unconditional`. |
| `update RESOURCE --patch JSON` | Apply ordered `add`, `replace`, and `remove` property operations to one Memory or informational Link. Accepts literal JSON, `@file`, or explicit `@-` stdin. Requires a Resource guard; the Memory source guard is optional. |
| `delete BEAD` | Read-only preview of deleting one unreferenced Memory. `--force` applies and requires `--if-revision TOKEN` or `--unconditional`. A preview needs no guard but checks any supplied guard. |
| `forget BEAD` | Apply the same unreferenced Memory deletion immediately, with `--if-revision TOKEN` or `--unconditional`. |
| `create TITLE [--id ID]` | Create an Issue. Bare IDs resolve under `beads/`. Allows `--title`, inline `--description`/`--body`/`--message`, `--type`, `--priority`, `--labels`/`--label`, inline `--design`, `--acceptance`, `--assignee`, `--estimate`, `--external-ref`, `--spec-id`, `--due` and initial `--notes`. Existing classification rules apply. Initial status is open. Ordinary creator identity and git-email Owner defaults are included in the Issue data. |
| `update BEAD` with Issue scalar flags | Inline `--title`, `--description`/`--body`/`--message`, `--design`, `--acceptance`, `--priority`, non-claim `--assignee`, `--estimate`, `--external-ref`, `--spec-id`, `--due` and literal `--append-notes`. Requires `--if-revision TOKEN` or `--unconditional`. Description aliases must agree. Files/stdin and other Issue fields are unavailable. |
| `update BEAD --claim` | Atomically claim one Issue for the current actor using the native writer. Standalone `--claim=true` only; no other edits or revision/force guard. Repeating the same actor is a no-op and does not renew its five-minute lease. |
| `show RESOURCE` | Current Memory, Issue or Link; optional `--version TOKEN` selects an exact retained record. Use `versions` to list a Resource's versions in order. |
| `versions RESOURCE` | List one Memory, Issue or Link's retained **citable Resource states** newest first, each with its store-local `local_revision`, version token, change time and actor. A removed Link still lists its prior live versions, but deletion does not create a Link version. Human output labels the number `REV`. In a graph workspace `history RESOURCE` is an alias with the same output; ordinary workspaces keep the Dolt-commit `history`. The pinned integration build still exposes a non-citable deletion-marker row; [draft fork PR #73](https://github.com/donnabox/beads/pull/73) corrects that gap, pending combined-source qualification. |
| `compare RESOURCE --from TOKEN --to TOKEN` | Compare two complete retained preview versions of one Memory, Issue or Link. Explicit tokens determine direction, not chronology. |
| `link SOURCE TARGET --link-type TYPE` | Use an installed Link Type as `types/NAME` or its full local URL. Informational Links permit `--id links/PATH`, `--properties JSON` and source guards. Memory sources own informational Links; Issue sources do not. |
| `dep add SOURCE TARGET` or `link SOURCE TARGET` | A local blocking Dependency between Issues, using the ordinary default `blocks` type. No bulk, remote, routing or bypass flags. |
| `update LINK --properties JSON` | Replace all informational Link properties. Requires a Link guard; the Memory source guard is optional. Blocking Dependency properties are not editable here. |
| `links BEAD` | Complete bounded current incident Links, with optional `--direction in\|out\|both` and `--link-type TYPE` filter. No pagination. |
| `unlink LINK` | Remove one informational Link or blocking Dependency by canonical ID. Requires a Link guard; the source guard is optional for Memory-owned Links, required for blocking Dependencies. |
| `unlink SOURCE TARGET --link-type TYPE` | Remove an unambiguous informational Link with the same guards. Multiple matches refuse and report candidate IDs. Blocking Dependency pair removal is unavailable. |
| `close [BEAD...]`, alias `done` | Ordinary interactive last-touched fallback or explicit one/many Issue IDs; reason aliases or `--reason-file`, `--force`, `--session`, `--suggest-next`, `--claim-next`, `--continue`, `--no-auto` and the ordinary conditional-revision path. Native gate, molecule and next-work effects apply. The pinned integration build supports only one explicit ID and inline reason. |
| `reopen BEAD...` | Reopen one or more Issues with optional `--reason`, clearing `closed_at` and recording the native Reopened event for accepted changes. The pinned integration build supports one explicit ID. |
| `ready` | Ordinary blocker-aware Issue query, all [target flags](#target-operations-that-remain-nyi) and atomic `--claim` behavior. The pinned integration build admits only an unfiltered read. |
| `list` or `list --format records-json` | Without an Issue filter, one bounded current snapshot of every Memory and every Issue the ordinary `bd list` would show: closed and pinned Issues are hidden unless `--all`, which also lifts the row limit. Newest recorded change first, ties by canonical Bead ID; human rows show local ID and kind, and for an Issue its status and priority, then the title. `--bead-type types/NAME` narrows by nominal Bead Type. A positive `BEADS_MAX_ROWS` refuses a page of more Beads than the cap. Any Issue filter (status/state, type, title/title-contains, priority and range, assignee/no-assignee, label/label-any/exclude-label, pinned/no-pinned, due-before/due-after/overdue, sort or reverse, or a matching configured directory label) selects the native Issue-only query, which lists Issues only, says so under the header in human output, and omits closed and pinned Issues unless `--all` or a filter selects them. See [All-Bead listing](#all-bead-listing). `hasMore` reports whether a row limit omitted matches; tree and legacy JSON remain unavailable. |
| `blocked` | Native dependency-blocked Issue view with canonical blocker IDs and ordinary `--parent` descendant filter. The pinned integration build has no parent filter and refuses positive `BEADS_MAX_ROWS`. |
| `graph BEAD --view generic` | Current local summary traversal with `--direction in\|out\|both`, `--depth`, `--max-nodes` and `--max-links`. |
| `status --graph` | Report the capabilities and bounds admitted by this checkpoint, including initial Issue fields/notes, append-only notes, estimate/reference edits and due-date authoring/filtering. |
| `serve --readonly --addr HOST:PORT` | BDP Read over HTTP for an ordinary shared-server graph workspace. Existing token-file authentication, Host controls and non-loopback opt-in apply. Embedded serving is refused. |
| `setup claude [--project] [--check\|--remove]` | Add, check or remove only the project-local Claude Stop hook (`bd claude-hook stop`); adding also ensures `CLAUDE.md` imports the graph guidance. Adding and `--check` require current graph-preview guidance. Global/stealth setup, other recipes and `--json` are unavailable. |
| `claude-hook stop` | Steph's Stop reminder, which Claude Code runs with its JSON hook input on stdin; it does not open storage. If graph admission refuses it (for example, `BD_BACKEND` is set or the workspace was moved), it prints one warning line and exits 1, which Claude Code does not treat as blocking. |

Commands accept the common graph controls `--json`, `--graph-mode`, `--actor`,
`--quiet`, `--no-color`, `--directory` and `--readonly` where applicable. A
read-only invocation cannot write. Unsupported command options refuse rather
than falling through to ordinary storage operations.

`create` defaults to an Issue. `--bead-type types/preview-memory-v2` selects a
Memory; `types/preview-issue-v2` explicitly selects an Issue. Full local Type URLs
also work. `--type` remains Issue classification (such as `task` or `bug`). Memory
creation accepts an optional ID, positional title or `--title`, and inline
`--description`/`--body`/`--message`. Without a title, body text supplies the summary.
Issue-only fields are refused on Memory creation. For example:

```sh
bd create --bead-type types/preview-memory-v2 --body 'Code flow policy: target integration.'
```

Run `bd types` in the selected graph workspace to see the Type IDs usable with
`--bead-type` and `--link-type`; `bd types --details` displays the full stored
descriptors. This graph Type catalog is distinct from the ordinary Issue
classifications selected by `bd create --type`.

New workspaces install three informational Link Types: `types/preview-related-v2`,
`types/example-follows` (the source follows a policy described by the target),
and `types/example-cites` (the source cites the target as context). All accept
Memory or Issue endpoints and optional `note` properties; none affects scheduling.
The blocking Type is `types/preview-blocks-v1` and requires Issue endpoints.
Use scope-relative `types/NAME` or the full local Type URL. Existing four-Type
workspaces remain readable and writable with their original Types; reads do not
install the two examples. Arbitrary Type installation is unavailable.
`--resource-type` remains a hidden compatibility alias for `--link-type`; do not
supply both.
For example:

```sh
bd remember 'Context for the plan.' --id beads/context --title Context
bd create 'Ship the release' --id beads/work --priority 1
bd link beads/plan beads/context \
  --link-type types/preview-related-v2 \
  --id links/context --properties '{"note":"background"}'
bd link beads/plan beads/work \
  --link-type types/preview-related-v2 \
  --id links/work
bd links beads/plan --json
bd update links/context --properties '{"note":"revised background"}' \
  --unconditional
bd unlink links/context --unconditional
```

`--unconditional` explicitly accepts the current record; use an observed
`--if-revision TOKEN` to reject a stale write. `remember --id` updates an
existing Memory unconditionally by default; `--update` remains the existing-only
spelling, while `--create-only` refuses any allocated ID. Property replacement,
property patches, Memory deletion and Issue edits still require an explicit
revision or unconditional choice. Source guards are
`--if-source-revision TOKEN` or `--unconditional-source`. Memory-owned Link
writes default to unconditional source acceptance; `--if-source-revision` opts
into stale-source refusal. Explicit `--unconditional-source` remains accepted.
The Link guard itself is still required for edits/removal, and blocking
Dependency removal still requires its Issue-source guard.
An unconditional changed Memory write, including an owned-Link change,
discloses the actual replaced version and attribution. Guarded writes and
semantic no-ops do not claim an unconditional overwrite. Targets do not acquire
new versions merely because another Bead links to them.

Unlink removes current Link state while reserving its identity and retaining
private prior snapshots. It does not erase a Memory or promise irreversible
erasure, restoration or identifier reuse.

## Ordered property changes

`--patch` applies a nonempty ordered array to the actual guarded predecessor
inside the existing write transaction. It cannot be combined with
`--properties`, selected Memory fields or Issue update flags. For example:

```sh
bd update beads/plan --if-revision OBSERVED_MEMORY_REVISION --patch \
  '[{"op":"replace","path":"/title","value":"Release plan"},{"op":"replace","path":"/body","value":"Updated context."}]' --json
bd update links/context --if-revision OBSERVED_LINK_REVISION \
  --if-source-revision OBSERVED_MEMORY_REVISION \
  --patch '[{"op":"add","path":"/note","value":"Updated background"}]' --json
bd update beads/plan --if-revision OBSERVED_MEMORY_REVISION --patch @changes.json
```

Use fresh observed tokens for each changed write. `--unconditional` accepts
the current Link itself. For a currently supported Memory-owned informational
Link, the source Bead separately defaults to accepting its current state;
`--unconditional-source` makes that choice explicit, while
`--if-source-revision` rejects a stale source. An Issue-source informational
Link does not currently require a source guard, but any supplied source guard
is checked. Extending ownership to another Bead Type requires a corresponding
source-guard contract; Memory is not the definition of ownership. Patching a
Link does not change its Issue source. Issue properties and blocking Dependency
properties cannot be patched through this route.

Operation objects contain only `op`, `path`, and, for add/replace, `value`.
Pointers use JSON Pointer escapes (`~0` for `~`, `~1` for `/`); an empty
pointer selects the root. Add inserts array items or replaces an object
member, and `-` appends to an array. Replace/remove require an existing
target; missing parents are never synthesized. Operations run in order and
may temporarily introduce arrays or other JSON values. The final Memory
properties must contain exactly the `title` and `body` strings; final Link
properties permit only an optional string `note`. Empty strings are present
values, distinct from absent members or null.

All operations succeed together or leave the stored state unchanged. A
same-value or reversing patch is a semantic no-op: the existing version,
revision and attribution remain unchanged, with no replacement disclosure.
Stale guards still refuse before no-op evaluation. A changed unconditional
write discloses the actual replaced Memory state, including when the change
is to a Memory-owned Link. Prior complete states remain available through
the exact retained reads described below. Deleted Memories and removed Links
cannot be revived by patching.

Input and each working properties document are limited to 1 MiB, with at
most 256 operations, 4,096 pointer bytes, and 64 pointer segments/nesting
levels. A cumulative 16 MiB evaluation-byte limit bounds repeated document
work; it is not an exact heap or timing bound. Duplicate members, invalid
Unicode, inexact unsupported numbers, extra operation members and unsupported
operations refuse. Files/stdin are read only when explicitly selected and
after readonly/freeze, selector and local guard admission. Input/parse errors
report `invalid_properties`; stale guards report `revision_conflict`.

Changed patches must also fit the complete current-read acquisition budget,
including retained final heads of deleted Memories. Failure rolls back the
mutation and retained records. Existing replacement/selected-field writers
keep their established admission policy. A no-op writes nothing and does not
repair an oversized workspace; a properties document already above the
patch limit cannot use this route to shrink itself. Runtime bound refusals
report `capability_unavailable`. These bounded CLI changes do not enable a
BDP Write profile, serve the draft BDP History surface or provide durable
request outcomes.

## Find and inspect retained Memory

`memories` searches current Memory titles and bodies using literal Unicode case
folding. It reads one checked current snapshot before filtering. The default
must match at most 50 Memories; a larger result refuses instead of returning a
partial page. Narrow the search or use `--all` for all matching summaries within
the same acquisition and output bounds. Default human output shows local IDs and
titles, plus labeled excerpts for body matches. `--details` adds exact versions,
attribution, owned-Link counts and recall commands. Structured `records-json`
output retains its full existing summary fields.

```sh
bd memories release
bd memories release --details --format records-json
# Use --details to copy an exact version for a retained read.
bd memories release --details
bd recall beads/plan --version SAVED_TOKEN
bd show beads/plan --version SAVED_TOKEN --json
bd compare beads/plan --from EARLIER_SELECTED_TOKEN --to OTHER_SELECTED_TOKEN --json
```

Structured summaries include identity, title, saved version, attribution, matched fields
and a body excerpt when the body matches. Search input is limited to 4,096 UTF-8
bytes, excerpts to 160 code points and final output to 1 MiB. A short matching
body can appear in full as its excerpt. Results are sorted by canonical ID,
marked complete and have no continuation. The existing 1,000 live Resource and
16 MiB acquisition bounds apply before filtering, including retained deleted
Memory heads counted by the current reader.

Use `--format records-json` for experimental summaries; `--json` has no settled
graph Memory-list mapping and refuses. Explicit `--format table` or
`--format records-json` takes precedence over ambient JSON configuration.
Ordinary KV-memory workspaces retain their existing output and `--format json`
alias. BDP Read remains the protocol interface for scripts.

`recall` writes the selected body's exact bytes with no envelope, added newline
or quiet suppression. Empty content succeeds with zero output bytes. Without
`--version`, it reads current Memory; with a token from a saved result it reads
that retained body even after edits or deletion. Graph JSON recall remains
unavailable because the complete Memory representation is unresolved.

`show --version` also supports retained Issue and Link records, including their
saved owned Links. Tokens are opaque nonempty UTF-8 strings of at most 4,096
bytes. `--version` does not interpret local ordinals, timestamps, full version
URLs or as-of selection; it takes only a token. An unknown or other-resource token returns `revision_unknown`;
a missing subject returns `not_found`. A removed Link's prior live versions
remain readable; its private removal token returns `gone`. A deleted Memory
retains its final live head without inventing a deletion version.

`compare` reads both operands in one authority-checked transaction. Equal tokens
still require a valid retained record. Either unavailable or corrupt operand
fails the whole operation. Differences include complete property values and
owned Links matched by canonical identity, with explicit presence so absence,
null and empty content remain distinct. Arrays retain order; object member
order is ignored; JSON numbers are compared without float rounding. Reversing
the tokens reverses the comparison. The selected versions and attribution are
context rather than property differences. Immutable identity or endpoint
mismatches refuse.

Comparison output is provisional JSON, indented for human output. Its
`compared` and `unsupported` fields describe the projection: common metadata and
Memory inception/derivation are unavailable. Issue comparison uses the native
retained durable projection; current row-lock and content-hash fields are not
reconstructed. Each retained snapshot acquisition has a 16 MiB input budget. A deleted
Memory also validates its final live snapshot before reading an older selected
snapshot, so a distinct pair can acquire up to 64 MiB in total (up to 32 MiB
for equal older tokens, which are resolved once). This excludes decoding/output
overhead and is not a total-heap or rendered-output cap. These read commands work under `--readonly` and migration freeze. They
do not order versions themselves (see `versions` below), and they do not enable
HTTP History, restoration or a stable public comparison contract;
`historyExact` remains false.

## List a Resource's versions

`versions` lists the retained versions of one Memory, Issue or Link, newest
first. In a graph workspace, `history` is an alias that produces the same
output. In an ordinary workspace `history` is unchanged and still reports
Dolt commits.

```sh
bd versions beads/plan
bd history beads/plan     # same output, graph workspaces only
# Feed a listed token to an exact read or a comparison.
bd show beads/plan --version LISTED_TOKEN
```

Each target row carries `local_revision`, `version`, `change_at`, `actor` and
`attribution`; `--json` reports them under those names, inside
a result that also names the `resource` and its `kind`. Human output labels
`local_revision` as `REV`, as ordinary `bd versions` does. The graph JSON
envelope and other row fields remain graph-specific. `version` is the opaque
token and the only citable address for a version. `local_revision` is an
ordering key, not an address: it is local to one Resource in one store and
cannot be passed to `show --version` or `compare`. The field is deliberately
not named `revision`, which already means the opaque token in graph records
and the row-lock token on native Issues. `change_at` is for display and is
never used to order rows, so two versions written within the same second
still list in write order. Memory and Link local revision numbers are allocated
per Resource when each version is written. Issue rows come from the native Issue
version record, use its local revision number and populate `attribution`
with its attribution status; Memory and Link rows leave `attribution` empty.
A deleted Memory's list ends at its final live head; deletion adds no version
to it.

Every target `versions` row is a citable Resource state: its token is accepted
by `show --version` and `compare`. Removing a Link retains its earlier states
and identity but mints no new Link version. The deletion belongs to the
history/event plane, not this Resource-version list. A private deletion marker
may remain in the store to retain identity and old snapshots, but it is not
returned as a `versions` row. The pinned integration build still emits that
marker as a non-citable `removed: true` row; draft fork PR #73 corrects the
CLI projection, pending combined-source qualification. No replacement deletion
version, timestamp or cascade is implied.

The command has two answers:

- An ordered list, newest first. A Resource's creation is its version 1,
  written in the same transaction that allocates it, so the list always has at
  least one row. Deleted Memories and removed Links still list their history.
- `not_found` when nothing was ever allocated at that path.

An allocated Resource with no versions is corruption, not an empty history,
and the command refuses rather than printing an empty list. Every plane in a
schema version 6 workspace can order its versions. A history too large to
return (more than 1,000 versions, or over the 16 MiB read budget) still
refuses with `capability_unavailable`, because limit refusals use that code;
there is no pagination.

Memory and Link ordinals are allocated as `MAX(ordinal)+1` per Resource inside
the writing transaction. Every graph write also updates one store-wide writer
fence, so two concurrent writers in one store always contend: the loser fails
at commit with `revision_conflict` (exit 4) and nothing is retained for it. A
unique `(path, ordinal)` key backs this up. Issue ordinals are native revisions
and are allocated by the native Issue writer.

This is a CLI listing only. The BDP v0 draft defines HTTP History, but this
command adds no HTTP route or conformance claim; current `serve` publishes no
History, and this list does not support as-of selection or restoration.
`status --graph` reports `versionList: true` for this command;
`historyExact: false` continues to describe the HTTP profile, where exact
History remains unavailable.

## Unreferenced Memory deletion

`delete` previews one current Memory without changing it, including under
`--readonly`. Apply with `delete --force` or `forget`; both require the existing
revision guard or an explicit unconditional choice. The `result.memory` in
both preview and apply is the checked final live Memory, including its existing
revision and attribution. `result.preview` distinguishes the read-only preview;
`result.deleted` is true only after successful apply. The outer `preview: true`
continues to identify the experimental graph format, including applied writes.

```sh
bd remember '' --id beads/scratch --title Scratch --json
bd delete beads/scratch --readonly --json
# Supply the revision observed in result.memory.revision from the preview.
bd delete beads/scratch --force --if-revision OBSERVED_REVISION --json
# Or apply to another unreferenced Memory with explicit unconditional intent:
bd remember 'Temporary note' --id beads/another-scratch --title Scratch
bd forget beads/another-scratch --unconditional --json
```

An empty body is present content and can be deleted. Successful deletion removes
current Memory state while preserving the canonical ID reservation and immutable
prior snapshots. A new `show` reports `gone`; recreating the same ID reports
`identity_reserved`. Repeated deletion refuses. Deletion does not create a new
live revision or a deletion version, and the result does not claim a deletion
timestamp or actor event. Internal snapshot retention does not expose public
History or restoration.

Every live incident Link causes both preview and apply to refuse with
`deletion_policy_unresolved`, including incoming, outgoing and self-Links.
Explicitly unlinking each Link with its normal guards allows a later deletion;
these are separate transactions, and a concurrently added Link can still cause
deletion to refuse. `--force` does not cascade. Batch selectors, `--from-file`,
`--cascade`, `--dry-run`, erasure and Issue deletion are unavailable in this
graph slice. Ordinary Issue deletion and key/value `forget` remain unchanged.
`status --graph` advertises `memoryUnreferencedDelete`; general `memoryDelete`
remains false because linked Memory deletion policy is unresolved.

## All-Bead listing

`bd list` has two modes, selected by its flags.

Without an Issue filter (bare `bd list`, or only `--flat`,
`--format records-json`, `--bead-type`, `--limit` and `--all`), the command
reads one checked current snapshot and lists its Beads: every current Memory
and every current Issue that the ordinary `bd list` would show. Closed and
pinned Issues are hidden, exactly as there: an Issue with status `closed` or
`pinned`, a set pinned flag, or a custom status in the done or frozen
category. `--all` shows them and also removes the row limit. A Memory has no
status and is never hidden.

Beads are ordered by when their current version was recorded, newest first,
and by canonical Bead ID where two share an instant. The order is the same
for every kind of Bead. It is a display order: a recorded time is a
wall-clock reading, not an acceptance-order token. Human output prints one
unquoted row per Bead: the local ID and kind, then for an Issue its status
and priority, then the title. For a workspace holding the Memory
`beads/plan` and the Issue `beads/work`, with `work` recorded last:

```text
Beads (2; more: false; graph preview)
  beads/work  Issue   open  P1  Ship the release
  beads/plan  Memory  Release plan
```

`--format records-json` returns the same Beads, in the same order, as
complete Memory and Issue records in `result.items`, with `result.hasMore`.
An optional `--bead-type types/NAME` keeps one installed Bead Type; Link
Types and uninstalled Types refuse with `capability_unavailable`, and a
selector that is neither `types/NAME` nor a full local Type URL refuses with
`invalid_selector`. `--bead-type types/preview-issue-v2` on its own stays in
this mode, so it hides closed and pinned Issues too unless `--all` is given.
`--limit N` returns the first N Beads and a truthful `hasMore`, not a
continuation cursor. A positive `BEADS_MAX_ROWS` refuses a page of more
Beads than that: it counts the Beads the command would print, after hiding
and after `--limit`, so a limit at or under the cap never trips it. The
snapshot inherits the 1,000-live-Resource/16 MiB acquisition bounds,
including Links that are not printed. For paginated all-Bead enumeration,
use the BDP HTTP `beads/` collection.

Any Issue filter selects the existing native Issue query instead. The Issue
filters are `--status` (or `--state`), `--type`, `--title`,
`--title-contains`, `--priority`, `--priority-min`, `--priority-max`,
`--assignee`, `--no-assignee`, `--label`, `--label-any`, `--exclude-label`,
`--pinned`, `--no-pinned`, `--due-before`, `--due-after`, `--overdue`,
`--sort` and `--reverse`. A supplied flag counts even when an empty value
such as `--assignee=` or `--sort=` adds no restriction. A configured
`directory.labels` entry that matches the current directory supplies an
implicit label filter, so it also selects this query, unless `--bead-type`
names a non-Issue Type: a label does not apply to a Memory, so the entry is
not applied there and nothing is refused. The query returns Issues only,
never Memories, and omits closed and pinned Issues unless `--all` or a filter
selects them. Human output says so on the line under the header:

```text
Issues (1; more: false; graph preview)
Memories are not listed (Issue query selected by: --status).
"https://example.org/team/beads/work" "open" P1 "Ship the release"
```

With a configured label the line reads
`Memories are not listed (Issue query selected by: directory.labels "frontend").`
Structured output carries no such line. A typed Issue filter cannot be
combined with a non-Issue `--bead-type`; that refuses with
`capability_unavailable`.

Both modes apply the ordinary `bd list` row limit. An explicit `--limit N`
wins, with `0` meaning no limit; otherwise `--all` means no limit; otherwise a
configured `list.limit` applies; otherwise there is no limit when output is
piped, 20 rows in agent mode at a terminal and 50 rows at other terminals.

## Read-only Issue queries and traversal

```sh
bd list --format records-json --status open --limit 1
bd list --flat --all --sort title
bd blocked --readonly --json
bd graph beads/plan --view generic --direction out --depth 2 --json
```

When an Issue filter (listed under [All-Bead listing](#all-bead-listing)) is
supplied, listing reuses the existing native Issue query, configuration and
limit policy.
It accepts status (or state), type, title/title-contains, priority and priority
range, assignee/no-assignee, label/label-any/exclude-label, pinned/no-pinned and
due-before/due-after/overdue filters. Sorting accepts
priority, created, updated, title, status or type; reverse is supported. Explicit
limit wins over `--all` and configured limits. The result contains complete
canonical Issue records in `items` and a truthful `hasMore` boolean. It is a new
read each time, not a snapshot cursor or BDP continuation. Returned records and
the extra probe record are validated before trimming the page.

Human output is flat by default; `--flat` is accepted and changes nothing.
The Issue query prints one row per Issue: its quoted canonical ID and status,
its priority and its quoted title, for example
`"https://example.org/team/beads/work" "open" P1 "Ship the release"`.
A line under the header says that Memories are not listed and names the
option that selected the query; structured output carries no such line.
`--format records-json` returns the experimental graph envelope. Tree output
(`--tree`, `--pretty` or `--flat=false`), `--json`, `--format json`, watch,
readiness, parent/ID/routing/offset selectors, repeated
status/state/type/assignee/bead-type filters and supplied-empty labels
refuse. Defer filters remain unavailable; due selection
does not schedule or wake work.
This does not change ordinary list parsing or implement contributor-owned filter
unions. Records-json selects structured errors and overrides ambient human
format selection; explicit `--json` still refuses. BDP Read remains the documented
script interface below; these CLI result shapes are experimental.

`blocked` follows the existing native dependency-blocked query. It is not the
complement of ready, nor a query for every manually blocked or deferred Issue.
The result is an array of objects with complete `issue` records and canonical
`blockedBy` IDs, sorted by canonical ID. Empty is `[]`. The Preview 2 target
accepts the ordinary `--parent` descendant filter but no positional selector.
The pinned integration build currently refuses all filters, including an
explicit empty `--parent`; a positive `BEADS_MAX_ROWS` also refuses there.
`blocked` and generic traversal do not wake deferred work, repair blocked state,
create versions or open the ordinary store. `ready` has the native lazy
defer-wake behavior described above. Readonly and migration freeze permit these
reads without authorizing a wake write.

Generic traversal reads one checked current snapshot. Nodes expose only ID,
Type, title, version and attribution; Links expose ID, Type, source, target,
version and attribution. Memory bodies, Issue long text and Link properties
are omitted. Directions are in/out/both; depth defaults to 1 and accepts 0..1000.
The root counts as one node. Default node/Link caps are 100/200, each accepting
1..1000. Distinct Link identities survive repeated-node visits, including diamonds
and self-Links. `frontier` identifies reached nodes with eligible unexpanded
Links; `complete` says whether that frontier is empty. Exceeding a node/Link cap
refuses rather than silently truncating. Foreign roots, external endpoints,
custom Types, legacy graph options and positive `BEADS_MAX_ROWS` refuse.

All three views retain the shared whole-workspace 1000-live-Resource and 16 MiB
acquisition bounds, including unrelated content and retained final heads of
deleted Memories. List config acquisition/resolved policy is additionally bounded
to 64 KiB and 256 rows/entries; database failures refuse instead of silently
choosing YAML fallback. List and blocked output have a 16 MiB bound; traversal
summary output has a 1 MiB bound. These are operational bounds, not heap guarantees.
Output is prepared before emission; physical output failures can still occur
after some bytes have been written.

A valid unreferenced Memory deletion does not invalidate an Issue query. Fresh
views exclude it, while its identity and final live retained state remain reserved.
Traversal cannot emit a deleted endpoint; a removed root is absent from its live
snapshot. Corrupt deleted allocations still refuse, including an invalid retained
head or an inconsistent current payload/Link. No new tombstone representation,
public History surface or external traversal contract is introduced.

## Priority and assignment

```sh
bd show beads/work --json
bd update beads/work --priority P0 --assignee alice --if-revision OBSERVED_REVISION --json
bd list --assignee alice --format records-json --all
bd update beads/work --assignee= --if-revision NEW_REVISION --json
bd list --no-assignee --format records-json --all
```

Use the opaque revision from the preceding `show` or accepted mutation. The
revision covers the Issue and its owned blocking Dependencies. Priority uses
ordinary P0–P4 meanings; omission preserves it, and zero explicitly sets P0.
Assignment preserves the literal value, including whitespace; an explicit empty
value clears it. Text, priority and assignment can be combined in one guarded
operation and one retained version. A matching same-value edit is a no-op;
a stale revision still refuses, even if every requested value is already current.

Assignment does not claim work, start a lease or change status. The ordinary
active-holder fence still applies to guarded and unconditional transfers. An
authorized transfer or clear removes the prior lease; unrelated edits preserve
it. Clearing an in-progress assignment does not reopen the Issue. Graph claim is
available only in the standalone form below; heartbeat and reclaim remain unavailable. Changes made through an external lease
writer have no supported graph-preview repair path.

Assignee listing reuses native SQL comparison/collation, not actor identity
normalization. `--no-assignee` includes native empty/NULL assignments. An empty
`--assignee=` supplies no assignee restriction but still selects the Issue
query; combining a nonempty assignee
with `--no-assignee` intersects the filters and returns no matches. Listing does
not modify state or claim work. Its output remains the experimental CLI envelope;
BDP HTTP Read is the script interface and exposes the current Issue properties.

## Append-only Issue progress

```sh
bd show beads/work --json
bd update beads/work --append-notes 'Finished the first pass.' --if-revision OBSERVED_REVISION --json
```

The native transaction appends to the current notes. Omission preserves notes;
empty on empty is a no-op, while empty on nonempty appends one newline. Text is
literal, including whitespace, Unicode, CRLF, `-` and `@file` markers; this flag
does not read files or stdin. Repeating nonempty text appends it again. A stale
revision refuses even when the append would otherwise be a no-op. An unknown
commit outcome must be inspected; never automatically replay an append.

Append can accompany supported scalar edits in one guarded update and one
retained version. Notes-only edits preserve owned Dependencies, claim lease
fields and closed state. Assignment mixed with append retains the native holder
fence and lease rules. A changed request supplying append intent must leave the
workspace within its existing read acquisition budget, including its new retained
snapshot, or every effect rolls back. This conservative postcondition also applies
to an empty append combined with a changing scalar. Scalar-only repair paths and
true no-ops keep their existing behavior. No new per-input notes limit is added.

Replacement/clear, force overwrite, and combined claim-plus-append remain
unavailable. Contributor notes replacement safeguards are a separate review gate.
CLI current/exact reads and BDP Read expose the accepted notes; no HTTP write or
public History contract is added.

## Standalone Issue claim

```sh
bd update beads/work --claim --actor alice --json
```

The native claim writer decides eligibility, including configured active statuses
and literal claim-pool aliases. A successful claim sets the assignee and
`in_progress` status, preserves an existing start time, and records one native
version and one complete graph version. It does not require the Issue to be ready.
A foreign holder or non-claimable status is refused without mutation.

The native lease lasts five minutes. Repeating the claim as the same actor
(including equivalent actor spelling) is a no-op, including the lease timestamps.
This preview does not provide heartbeat, renewal, reclaim, unclaim or lease repair.
An external writer that changes the lease bypasses the saved graph projection;
current graph reads and writes then refuse that mismatch. Previously retained
snapshots remain readable. Do not use external lease commands on these preview
Issues expecting the graph to repair them.

`--claim=false`, combined claim plus scalar/property edits, revision guards and
unconditional/force claim flags are refused. A lost commit reply reports an
unknown outcome; callers must inspect rather than automatically retry the write.

## Issue due dates

```sh
bd create 'Prepare the review' --id beads/review --due '2030-01-02T10:00:00Z' --json
bd update beads/review --due '+30min' --if-revision OBSERVED_REVISION --json
bd list --format records-json --due-after '2030-01-01' --due-before '2030-01-03'
bd list --format records-json --overdue --all
bd update beads/review --due= --if-revision OBSERVED_REVISION --json
```

Create and guarded update reuse the ordinary date parser, including `+30min`,
`+6h`, date-only and offset-bearing timestamps. `m` means calendar months;
`min` means minutes. Timezone-less input uses the invoking process's local zone.
Omission preserves the existing due date; explicit empty update clears it.
An empty create value leaves the date absent. Due edits may accompany other
admitted scalar edits or notes append in the same native transaction/version;
standalone claim still cannot be combined with an edit.

This private preview retains the existing whole-second SQL representation:
writes normalize to UTC and round to the nearest second, with half-second ties
forward. Filter cutoffs normalize to UTC and truncate fractions before native
strict `<` / `>` comparisons. A due time of `12:00:00Z` therefore does not match
a before-cutoff of `12:00:00.900Z`. Dates must remain within years 1..9999 after
normalization. These are provisional compatibility rules, not a durable BDP
precision contract. The CLI's 4096-byte UTF-8 date-input bound is likewise a
private preview admission limit.

The ordinary filters intersect, including existing status/type/limit policy.
Overdue means a non-null due date before the native UTC query clock, excluding
closed Issues even with `--all`; it neither changes status nor schedules work.
After normalization, an unchanged due value or repeated clear is a no-op, while
a stale revision still refuses. Current and exact retained reads preserve the
accepted instant; due writes retain existing links, lease and lifecycle state.
No defer, recurrence, mandatory deadline or scheduler behavior is added.

## Limits and remaining work

Deleting a linked Memory is refused on integration until the caller explicitly
unlinks it; `--force` does not bypass incident Links. Graph Issue deletion,
defer/scheduling, notes replacement/clear and label mutation remain unavailable
on integration. Draft PRs #105, #106 and #108 implement parts of the Issue
journey but have not landed or completed release qualification. Issue creation
can set initial labels; that does not adopt a label-editing contract. These
restrictions apply to graph workspaces; ordinary Issue workspaces keep their
existing behavior.

BDP HTTP Read is available on ordinary shared-server Dolt as described below.
CLI JSON is a separate command output format. No public History route is
enabled by the exact retained CLI reads or experimental comparison.

Writes retain their complete accepted state within the existing transaction.
Those snapshots are not a claim of complete native History, Dolt HEAD/sync
durability, public BDP Write or recovery support. The current read acquisition
budget can refuse an oversized workspace; this checkpoint does not promise
that every successful create preserves that aggregate budget. `status --graph`
reports the current bounds.

## BDP Read from scripts

A graph workspace on ordinary shared-server Dolt can expose its current records
through BDP HTTP. The protocol and independent public client are pinned to
`gastownhall/bdp@53bdbd03136875f952af184fce7b3c7af8f74e96`. This serves the Read
profile; it does not enable HTTP writes or History.

Choose a stable, reachable HTTP address **before** initializing a new workspace.
The persisted Scope URL supplies record identities and the HTTP path. It does
not change when an allowed Host alias or reverse proxy reaches the listener.
For a disposable local example, with an ordinary Dolt server already listening
on port 3306, use a new directory:

```sh
git init
bd init --graph-mode link --scope-url http://127.0.0.1:8765/demo/ \
  --server --external --server-host 127.0.0.1 --server-port 3306 \
  --server-user root --skip-hooks --skip-agents --non-interactive
bd remember 'Why we chose this design.' --id beads/plan --title Plan
bd serve --readonly --addr 127.0.0.1:8765
```

From another terminal, read actual BDP representations:

```sh
curl --fail-with-body -i http://127.0.0.1:8765/demo/
curl --fail-with-body http://127.0.0.1:8765/demo/bdp.json
curl --fail-with-body http://127.0.0.1:8765/demo/beads/plan
curl --fail-with-body 'http://127.0.0.1:8765/demo/beads/plan?view=properties'
curl --fail-with-body 'http://127.0.0.1:8765/demo/beads/?limit=1'
curl --fail-with-body http://127.0.0.1:8765/demo/types/preview-memory-v2
curl --fail-with-body 'http://127.0.0.1:8765/demo/beads/plan?view=links&direction=both'
```

The Scope root returns a `service-desc` Link to `bdp.json`. Collections expose
`items` and `next`; enumerate by following each complete `next` URL until it is
null. A first page alone is not the full inventory. Bead, Link and Type
collections are available at `beads/`, `links/` and `types/`. Bead/Link
collections support `type`, `conformsTo` and bounded `selector` filters; Link
collections also support `source`, `target` and `endpoint`. Incident HTTP
directions are `inbound`, `outbound` and `both`. `include=links` returns a Bead
and its first incident Link page from one snapshot.

GET and HEAD use canonical current identities. Resource/properties responses
carry revision ETags. Authentication, authority, query and storage validation
precede conditional responses: an invalid cursor or missing Resource cannot
become a successful 304. No Last-Modified timestamp is invented. Historical
version queries, aliases, HTTP mutation, legacy `/v0` and `/healthz` routes are
unavailable. Use an authenticated Scope or discovery request for readiness.

A collection continuation retains its original records and owned Link state
even if a later CLI write changes fresh reads. Cursors expire after five
minutes and do not survive service restart. This is retained pagination, not
chronological History. Stop the service before restoring storage; same-binding
hot restore and out-of-band SQL are unsupported.

The [standard-library Python example](https://github.com/versioned-beads/beads/tree/integration/examples/bdp-read)
follows every continuation and can also read one canonical Bead. From this
repository's root, with the example listener running:

```sh
python3 examples/bdp-read/read_beads.py --scope http://127.0.0.1:8765/demo/ --limit 1
python3 examples/bdp-read/read_beads.py --scope http://127.0.0.1:8765/demo/ \
  --id http://127.0.0.1:8765/demo/beads/plan
```

For an authenticated listener, supply its token through the `BDP_TOKEN`
environment variable. The example reads BDP HTTP directly and prints results
only after the requested read or full enumeration succeeds.

### HTTP access and limits

The existing `serve` controls apply: `--auth-token-file`, `--allowed-host`,
`--allow-non-loopback` and explicit `--insecure-no-auth`. The default listener
is loopback; its existing local trust policy permits no-token access there.
For a token-protected listener, provide a bearer token on every request,
including continuations. Token-file rotation keeps the existing last-good-file
reload policy. Every accepted token sees the complete workspace; separate
per-user authorization views are not implemented. Responses use
`Cache-Control: private, no-store`. The server provides no TLS or CORS support.

The store stays open until HTTP requests have drained. Serving opens only an
already initialized, matching ordinary shared-server graph authority; it does
not create a workspace or enable legacy writers. Embedded CLI operations remain
supported, but embedded HTTP serving refuses before opening storage or binding
a listener.

Page size defaults to 100 and is capped at 1,000. The current inventory is
limited to 1,000 live Resources and a conservative 16 MiB persisted-byte
acquisition budget before filtering. That budget can refuse a small or exact
read because other current data is oversized. HTTP representations are limited
to 8 MiB and retained snapshots to 7 MiB; at most 32 snapshots, 4,096
continuation positions and 32 MiB total are retained. Request targets are capped
at 32 KiB, Selectors at 16 KiB, depth 256 and 2,048 nodes. Capacity pressure
refuses new snapshots without evicting valid continuations. These are preview
bounds, not a guarantee about Go heap usage or production migration.

## Regression coverage

The required Graph C0 gate checks source/CLI provenance, storage tests on both
engines, graph configuration and CLI admission with no required skips. Its
installed initialization and fresh-process read checks remain in place. The
mixed workflow adds real CLI coverage for Memory edits, all three informational
endpoint combinations, Link replacement/unlink, blocking readiness and Issue
close/reopen/text editing. Storage tests verify retention, guards, no-ops,
concurrency, rollback and uncertain commit outcomes separately. Ordinary Issue
regression suites remain required. Qualification of any new combined commit
requires the actual destination CI results, not this document alone.

BDP coverage includes storage-to-wire projection on both real engines, bounded
selectors and retained pages, plus an ordinary-server HTTP listener exercising
authentication, token rotation, Host identity, authority loss, conditionals and
concurrent aggregate reads. The independent public-client capture uses the
installed CLI as its writer and real HTTP as its reader. Aggregate/replay
checks use real fetch plus the pinned public parsers where the public client
has no corresponding API. Combined-source qualification must include these
roots and captures without required skips; internal projection tests alone
are not installed HTTP proof.

The unreferenced deletion workflow adds normal installed CLI initialization on
both engines, empty-body preview/apply, stale and missing guard refusals,
read-only admission, fresh-process absence, ID nonreuse, incident-Link refusal
and explicit unlink before deletion. These tests do not implement or qualify
the unresolved linked-deletion policy.

## Issue authoring fields

```sh
bd create 'Implement the plan' --id beads/work --design 'Approach' --acceptance 'Done when verified' --assignee alice --estimate 45 --external-ref 'tracker #42' --spec-id 'spec/section' --notes 'Initial context' --json
bd update beads/work --estimate 0 --external-ref= --spec-id= --if-revision OBSERVED_REVISION --json
bd update beads/work --append-notes 'First pass complete.' --if-revision NEW_REVISION --json
```

Initial fields are part of one native create and retained record. Initial assignment
is not a claim and does not create a lease. Existing Owner and CreatedBy defaults
still apply, including the ordinary git-email Owner data already disclosed above.
Omitted estimates remain absent; explicit zero is present. Estimates use the
existing nonnegative signed SQL INT range; clearing an estimate to NULL is not
admitted. External and spec references are literal strings, not fetched URLs or
alternate Bead identities. The existing columns allow 255 and 1024 Unicode
codepoints respectively. Empty external reference on update clears it to NULL;
empty spec ID clears it to an empty string. CLI create omits an empty external
reference; the Go API preserves an explicitly supplied empty pointer.

Design, acceptance and initial notes are literal UTF-8, including whitespace and
line endings. Changed initial fields must survive storage hydration exactly.
Nonempty initial notes also require the completed current and retained state to
fit the existing workspace read budget before commit. Later notes changes use
append only; replacement and clear remain unavailable. Guarded scalar edits keep
owned Links and unrelated fields unchanged; an identical scalar edit records no
new version. Current CLI records, exact retained records and BDP Read carry these
fields without a new wire representation or HTTP write operation.

## Differences from the Memory Beads proposal

The [Memory Beads proposal, Revision 4](https://github.com/gastownhall/beads/issues/5877)
describes a broader product target. Its R31 table identifies command changes,
while other requirements define the Memory and shared-graph behavior those
commands need. This draft describes the target graph CLI and separately pins
the current integration subset. The differences below are **not** claims that the proposal's target
has shipped or that this draft supersedes it. Reconciliation is a product
decision; an NYI label alone does not settle a conflicting contract.

| Topic | Memory Beads proposal | This CLI specification and current build |
| --- | --- | --- |
| Human names and lookup | R2, R25 and R31 allow zero or more human keys for a canonical Memory and recall by key or ID; safe legacy-key conversion is also part of the target. | Graph selectors use a canonical Bead ID or exact local URL. No key or alias is created, resolved or migrated. Optional `--id` chooses identity, not a human key. This is an unresolved compatibility gap. |
| Memory discovery | R6, R25 and R31 call for deterministic compact search with honest continuation/completeness, plus `bd memories --json` returning one complete, body-carrying record per Memory, including its keys. | `bd memories` returns bounded summaries in a single complete result with no cursor. It refuses `--json`; `--format records-json` is a summary form, and full body reading uses `bd recall`. The JSON spelling, complete-record form and continuation policy need reconciliation. |
| Generic Bead listing | R31 says generic `bd list` can include Memory records identified by kind, while task-workflow filters do not select Memories; it does not prescribe a filter flag. | `bd list --bead-type types/preview-memory-v2` selects the installed Memory Type; `--type` remains an Issue classification filter. The generic listing goal is represented, but the CLI filter vocabulary is this draft's choice. |
| Recall and structured inspection | R7 settles the target structured payload: Project ID, Bead ID, keys, the selected state's pinned Reference, attribution, inception, derivation, metadata, outgoing Links and complete body. R31 also requires exact historical recall. Neither ruling selects a `bd recall --json` flag. | `bd recall` emits exact body bytes and refuses `--json`; `bd show --json` supplies a structured current or retained record separately, without claiming the complete R7 payload. The CLI spelling for the required combined structured read remains open. |
| Link vocabulary and addressing | R17, R23 and R31 use `bd link --type related` as the baseline informational relationship and allow user-defined non-workflow Types on Memory endpoints. References can name `latest` or a pinned version, including a cross-Scope target without resolving it. | `--link-type types/NAME` chooses an installed informational Link Type; `--type` retains its blocking Dependency meaning. The current workspace has only local, current-state endpoints, no pinned or cross-Scope Reference writing, and no Type installation CLI. The default vocabulary and flag compatibility need a decision. |
| Removal, erasure and restoration | R5, R20 and R31 make `forget` an immediate recoverable disappearance; deletion removes Links following `latest`, retains pinned Links, and reports removed Links and released keys. The proposal also distinguishes `delete --erase --force` and restoration. | `forget` requires `--if-revision` or explicit `--unconditional`. Any incident Link blocks current Memory deletion until explicitly unlinked; graph Issue deletion is NYI. No cascade, erasure, restoration, key release or deletion version is promised. The linked-deletion policy is a deliberate safety boundary here and conflicts with the proposal's target. |
| Agent context | R8, R25 and R31 replace `bd prime` body injection with guidance for selective discovery and recall. | Graph `bd prime` is refused. Generated agent guidance and the CLI read commands cover part of that workflow, but the proposed `prime` compatibility behavior is not implemented. |
| Creation validity | R4 requires at least one of title or body to contain non-whitespace text on creation. | `bd remember` currently permits a whitespace-only body and derives an empty title. That admitted edge case differs from the proposal and needs an explicit validation decision. |
| Shared capabilities and bulk surfaces | R3, R13, R18–R21, R27 and R27a require richer Reference, History, metadata/derivation, interchange and export behavior with explicit body-exposure posture. | Local exact versions and comparison exist, but public HTTP History, pinned References, restoration, interchange, Memory export and full metadata/derivation are not supplied by the current CLI. Proposed open metadata is NYI above; Type lifecycle and shared History remain separate contracts. |
