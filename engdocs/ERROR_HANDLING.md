# Error Handling Guidelines

Last reviewed: 2026-10-06

Freshness source: `cmd/bd/*.go`, especially command error exits and JSON error
helpers in `cmd/bd/errors.go` and the graph admission path in
`cmd/bd/graph_preview.go`.

This document describes the ordinary CLI's error handling patterns and gives
guidelines for choosing among them. Graph-mode commands additionally use the
typed `graphFailure` contract in `cmd/bd/graph_preview.go`; its exit categories
and stderr envelope are specified in the
[Graph CLI Specification (Draft)](../docs/reference/graph-cli-specification-draft.md#results-errors-and-repeatability).
Do not translate a graph refusal into an ordinary `HandleError` exit 1 or a
success-shaped JSON result.

## Overview

The beads codebase currently uses **three distinct error handling patterns** across different scenarios. Understanding when to use each pattern is critical for maintaining consistent behavior and a good user experience.

## The Three Ordinary CLI Patterns

### Pattern A: Return a Fatal Error Through `RunE` (`return HandleError(...)`)

**When to use:**
- **Fatal errors** that prevent the command from completing its core function
- **User input validation failures** (invalid flags, malformed arguments)
- **Critical preconditions** not met (missing database, corrupted state)
- **Unrecoverable system errors** (filesystem failures, permission denied)

**Example:**
```go
if err := store.CreateIssue(ctx, issue, actor); err != nil {
    return HandleError("%v", err)
}
```

**Why not `os.Exit(1)`?** Calling `os.Exit` inside a command handler abandons the
stack without running deferred functions — the per-command metrics event
(`CloseEventAndAdd`) and `main()`'s `metrics.CloseAndFlush()` never run, so the
invocation records no usage event and any `defer`red cleanup (unit-of-work close,
temp-file removal) is skipped. Instead return a `HandleError*` value: it prints
the message and returns a sentinel `*exitError`; cobra unwinds the stack (running
every `defer`), and `main()` exits with the sentinel's code (1 for every
`HandleError*` helper).

**Characteristics:**
- Prints `Error:` (plus `Hint:` for the `WithHint` variants) to stderr; under
  `--json` the `RespectJSON` variants emit a structured JSON error to stdout
  instead, `HandleErrorWithHint` emits it to stderr, and plain `HandleError`
  has no JSON mode
- Returns `&exitError{Code: 1}` up through `RunE`; `main()` exits 1 after
  deferred cleanup and the metrics flush have run
- The command's `cobra.Command` **must** set `SilenceUsage: true` and
  `SilenceErrors: true`, or cobra will additionally print `Error: exit code 1`
  and the usage text on top of the real message

**Narrow exceptions still using `os.Exit`:** process-level gates that run before
or outside the `RunE` error path — e.g. `CheckReadonly`, which aborts a blocked
command after flushing metrics first (exit 1 for read-only mode,
`ExitMigrationFrozen` (14) for a migration freeze), plus `main()` itself and its
graceful-shutdown handler. The one handler body that still calls `os.Exit` is
the hidden `send-metrics` flusher, which must not fall through to `main()`'s
post-command tail. New command code should return a `HandleError*` value
instead of adding more.

**Files using this pattern:** most commands in `cmd/bd/` — search for
`return HandleError` (e.g. `create.go`, `defer.go`, `dolt.go`, `unclaim.go`,
`compact.go`).

---

### Pattern B: Warn and Continue (`fmt.Fprintf` + continue)

**When to use:**
- **Optional operations** that enhance functionality but aren't required
- **Metadata operations** (config updates, analytics, logging)
- **Cleanup operations** (removing temp files, closing resources)
- **Auxiliary features** (git hooks installation, README generation)

**Example:**
```go
if err := createConfigYaml(beadsDir, false, ""); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to create config.yaml: %v\n", err)
    // Non-fatal - continue anyway
}
```

**Characteristics:**
- Writes `Warning:` prefix to stderr
- Includes context about what failed
- Command continues execution
- Core functionality still works

**Files using this pattern:**
- `cmd/bd/init.go` (search for `Warning:`)
- `cmd/bd/sync_push_pull.go` (search for `Warning:`)
- `cmd/bd/create.go`, `cmd/bd/gc.go` (search for `WarnError(`)

These auxiliary failures can be reported without hiding a failed core
operation.

---

### Pattern C: Silent Ignore (`_ = operation()`)

**When to use:**
- **Resource cleanup** where failure doesn't matter (closing files, removing temps)
- **Idempotent operations** in error paths (already logging primary error)
- **Best-effort operations** with no user-visible impact

**Example:**
```go
_ = store.Close()
_ = os.Remove(tempPath)
```

**Characteristics:**
- No output to user
- Typically in `defer` statements or error paths
- Operation failure has no material impact
- Primary error already reported

**Files using this pattern:**
- `cmd/bd/init.go` (search for `_ = store.Close()`)
- `cmd/bd/backup_export.go` (search for `_ = os.Remove(`)
- `cmd/bd/setup/utils.go` *(temp-file cleanup in the atomic-write helper)*
- Dozens of other locations throughout the codebase

---

## Decision Tree

Use this flowchart to choose the appropriate error handling pattern:

```
┌─────────────────────────────────────┐
│ Did an error occur?                 │
└─────────────┬───────────────────────┘
              │
              ├─ NO  → Continue normally
              │
              └─ YES → Ask:
                       │
                       ├─ Is this a fatal error that prevents
                       │  the command's core purpose?
                       │
                       │  YES → Pattern A: return HandleError(...) from RunE
                       │        • Prints "Error: ..." to stderr
                       │        • Provide actionable hint (HandleErrorWithHint)
                       │        • Returns *exitError; main() exits 1 after defers
                       │
                       ├─ Is this an optional/auxiliary operation
                       │  where the command can still succeed?
                       │
                       │  YES → Pattern B: Warn and continue
                       │        • Write "Warning: ..." to stderr
                       │        • Explain what failed
                       │        • Continue execution
                       │
                       └─ Is this a cleanup/best-effort operation
                          where failure doesn't matter?

                          YES → Pattern C: Silent ignore
                                • Use _ = operation()
                                • No user output
                                • Typically in defer/error paths
```

## Examples by Scenario

### User Input Validation → Pattern A (Return Fatal Error)

```go
priority, err := validation.ValidatePriority(priorityStr)
if err != nil {
    return HandleError("%v", err)
}
```

### Creating Auxiliary Config Files → Pattern B (Warn)

```go
if err := createConfigYaml(beadsDir, false, ""); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to create config.yaml: %v\n", err)
    // Non-fatal - continue anyway
}
```

### Cleanup Operations → Pattern C (Ignore)

```go
defer func() {
    _ = tempFile.Close()
    if writeErr != nil {
        _ = os.Remove(tempPath)
    }
}()
```

### Optional Metadata Updates → Pattern B (Warn)

```go
if err := store.SetMetadata(ctx, "last_import_time", time.Now().Format(time.RFC3339)); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to initialize last_import_time: %v\n", err)
}
```

### Database Transaction Failures → Pattern A (Return Fatal Error)

```go
if err := store.CreateIssue(ctx, issue, actor); err != nil {
    return HandleError("%v", err)
}
```

## Anti-Patterns to Avoid

### ❌ Don't mix patterns inconsistently

```go
// BAD: Same type of operation handled differently
if err := createConfigYaml(dir, false, ""); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: %v\n", err) // Warns
}
if err := createReadme(dir); err != nil {
    fmt.Fprintf(os.Stderr, "Error: %v\n", err)
    os.Exit(1) // Exits - inconsistent!
}
```

```go
// GOOD: Consistent pattern for similar operations
if err := createConfigYaml(dir, false, ""); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to create config.yaml: %v\n", err)
}
if err := createReadme(dir); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to create README.md: %v\n", err)
}
```

### ❌ Don't silently ignore critical errors

```go
// BAD: Critical operation ignored
_ = store.CreateIssue(ctx, issue, actor)
```

```go
// GOOD: Return a fatal error through RunE
if err := store.CreateIssue(ctx, issue, actor); err != nil {
    return HandleError("%v", err)
}
```

### ❌ Don't exit on auxiliary operations

```go
// BAD: Exiting when git hooks fail is too aggressive
if err := installGitHooks(); err != nil {
    fmt.Fprintf(os.Stderr, "Error: %v\n", err)
    os.Exit(1)
}
```

```go
// GOOD: Warn and suggest fix
if err := installGitHooks(); err != nil {
    fmt.Fprintf(os.Stderr, "\n%s Failed to install git hooks: %v\n", ui.RenderWarn("⚠"), err)
    fmt.Fprintf(os.Stderr, "You can try again with: %s\n\n", ui.RenderAccent("bd doctor --fix"))
}
```

## Testing Considerations

When writing tests for error handling:

1. **Pattern A (Fatal)** - Assert `RunE` returns a non-nil error (a `*exitError`); no subprocess or `os.Exit` mock needed, since `HandleError` returns rather than exiting
2. **Pattern B (Warn)** - Capture stderr and verify warning message
3. **Pattern C (Ignore)** - Verify operation was attempted, no error propagates

## Common Pitfalls

### Metadata Operations

**IMPORTANT:** Not all metadata is created equal. There are two distinct categories with different error handling requirements:

#### Configuration Metadata (Pattern A: Fatal)

Configuration metadata defines **fundamental system behavior** and must succeed:

```go
// Pattern A: return a fatal error through RunE.
// Returning lets a single `defer store.Close()` cover every exit path, instead
// of repeating a manual `_ = store.Close()` before each os.Exit (which os.Exit
// would otherwise skip).
defer store.Close()

if err := store.SetConfig(ctx, "issue_prefix", prefix); err != nil {
    return HandleError("failed to set issue prefix: %v", err)
}

if err := store.SetConfig(ctx, "sync.branch", branch); err != nil {
    return HandleError("failed to set sync.branch: %v", err)
}
```

**Examples:**
- `issue_prefix` - Defines how all issue IDs are generated
- `sync.branch` - Critical for git synchronization workflow

**Rationale:** These settings are prerequisites for basic operation. Without them, the system cannot function correctly. A failure here indicates a serious problem (e.g., filesystem issues, database corruption).

#### Tracking Metadata (Pattern B: Warn and Continue)

Tracking metadata **enhances functionality** but the system works without it:

```go
// Pattern B: Warn and continue
if err := store.SetLocalMetadata(ctx, "bd_version", Version); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to write bd_version local metadata: %v\n", err)
    // Non-fatal - continue anyway
}

if err := store.SetMetadata(ctx, "repo_id", repoID); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to set repo_id: %v\n", err)
}

if err := store.SetMetadata(ctx, "last_import_time", time.Now().Format(time.RFC3339)); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to initialize last_import_time: %v\n", err)
}
```

**Examples:**
- `bd_version` - Enables version mismatch warnings on upgrades (clone-local, so written with `SetLocalMetadata`)
- `repo_id` / `clone_id` - Helps with collision detection across clones
- `last_import_time` - Marks the database as synced at init (client-local sync state)

**Rationale:** System degrades gracefully if tracking metadata is unavailable. Core functionality (creating issues, importing data) still works. Failures here might indicate temporary issues (e.g., read-only filesystem) that shouldn't block the entire operation.

**See also:** `cmd/bd/init.go` (search for `CONFIGURATION METADATA` and `TRACKING METADATA`) for detailed inline documentation of this distinction.

### File Permission Errors

Setting file permissions is typically **Pattern B** because the file was already written:

```go
if err := os.Chmod(jsonlPath, 0600); err != nil {
    fmt.Fprintf(os.Stderr, "Warning: failed to set file permissions: %v\n", err)
}
```

### Resource Cleanup

Always use **Pattern C** for cleanup in error paths:

```go
defer func() {
    _ = tempFile.Close()      // Pattern C: already handling primary error
    if writeErr != nil {
        _ = os.Remove(tempPath) // Pattern C: best effort cleanup
    }
}()
```

## Enforcement Strategy

### Code Review Checklist

- [ ] Fatal errors use Pattern A with descriptive error message
- [ ] Optional operations use Pattern B with "Warning:" prefix
- [ ] Cleanup operations use Pattern C (silent)
- [ ] Similar operations use consistent patterns
- [ ] Error messages provide actionable hints when possible

### Error Helpers

`cmd/bd/errors.go` provides the shared helpers that enforce consistency. Pattern A
handlers return one of the `HandleError*` values; Pattern B can use `WarnError`
(most existing code writes the `Warning:` line to stderr directly instead):

```go
// Return through RunE — prints "Error: ..." to stderr, returns *exitError{Code: 1}
func HandleError(format string, args ...interface{}) error

// Like HandleError, but emits a structured JSON error to stdout under --json
func HandleErrorRespectJSON(format string, args ...interface{}) error

// Adds a "Hint: ..." line (under --json the plain variant writes the JSON error
// to stderr and the …RespectJSON variant writes it to stdout)
func HandleErrorWithHint(message, hint string) error
func HandleErrorWithHintRespectJSON(message, hint string) error

// Typed capability refusal from the proxied-server front doors — "Error: ..." to
// stderr, or under --json a JSON error carrying code, error, mutates (and reason,
// when the registry gave one) to stdout; returns *exitError with the refusal's
// own exit code (1 when it has none)
func HandleProxyCapabilityError(err error) error

// Exit 1 with no message, when the error was already reported
func SilentExit() error

// Pattern B — prints "Warning: ..." to stderr and returns nothing
func WarnError(format string, args ...interface{})
```

A command that needs an exit code other than 1 returns `&exitError{Code: N}`
rather than calling `os.Exit(N)` — for example `ExitMigrationFrozen` (14) in
`cmd/bd/errors.go`, `ExitSyncConflict` (2) in `cmd/bd/sync.go`, and
`ExitRemoteDivergenceRefused` (10) in `cmd/bd/init_safety.go`.

## Related Issues

- **bd-9lwr** - Document inconsistent error handling strategy across codebase (this document)
- **bd-bwk2** - Centralize error handling patterns in storage layer
- Future work: Audit all error handling to ensure pattern consistency

## References

- `cmd/bd/errors.go` - The `HandleError*` / `HandleProxyCapabilityError` / `WarnError` / `SilentExit` helpers and the `exitError` sentinel that `main()` maps to an exit code
- `cmd/bd/defer.go` - Clean example of Pattern A: `return HandleError(...)` from a `RunE` with `SilenceUsage`/`SilenceErrors` set
- `cmd/bd/init.go` - Examples of Patterns B and C and of configuration vs. tracking metadata; its fatal errors are plain `fmt.Errorf` returns (or `*exitError` values), not `HandleError*`
- `cmd/bd/migrate.go` - Examples of Pattern B for metadata operations (`repo_id`) and Pattern A for config updates (`sync.branch`)
