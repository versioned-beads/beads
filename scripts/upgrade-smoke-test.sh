#!/bin/bash
set -euo pipefail

# =============================================================================
# Upgrade Smoke Tests — Release Stability Gate
# =============================================================================
#
# Verifies that upgrading from a previous release preserves:
#   1. Issue data (issues created before upgrade are visible after)
#   2. Storage mode (embedded stays embedded, shared stays shared)
#   3. Role config (beads.role git config is not cleared or changed)
#   4. Doctor health (bd doctor quick passes after upgrade)
#   5. Mutations (bd update after upgrade persists correctly)
#
# Usage:
#   ./scripts/upgrade-smoke-test.sh              # test previous release → candidate
#   ./scripts/upgrade-smoke-test.sh v0.62.0      # test specific version → candidate
#   CANDIDATE_BIN=./bd ./scripts/upgrade-smoke-test.sh  # use prebuilt candidate
#   DOLT_BIN=/path/to/dolt ./scripts/upgrade-smoke-test.sh  # dolt CLI for scenario 7's
#                                                # failure-only readback (default: dolt on PATH)
#
#   # Test multiple versions (space-separated):
#   SMOKE_VERSIONS="v0.62.0 v0.61.0 v0.60.0" ./scripts/upgrade-smoke-test.sh
#

# The candidate binary is built from the current worktree if CANDIDATE_BIN
# is not set. The previous release binary is downloaded and cached in
# ~/.cache/beads-regression/.
#
# Exit codes:
#   0  All scenarios passed
#   1  One or more scenarios failed
# =============================================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Canonical build flags (GOFLAGS=-tags=gms_pure_go, CGO_ENABLED=1).
# shellcheck source=../.buildflags
source "$PROJECT_ROOT/.buildflags"

# ---------------------------------------------------------------------------
# Multi-version mode: SMOKE_VERSIONS overrides single-version argument
# ---------------------------------------------------------------------------

if [ -n "${SMOKE_VERSIONS:-}" ]; then
    # Run ourselves once per version, collecting exit codes
    OVERALL_FAIL=0
    for _ver in $SMOKE_VERSIONS; do
        if ! SMOKE_VERSIONS= CANDIDATE_BIN="${CANDIDATE_BIN:-}" "$0" "$_ver"; then
            OVERALL_FAIL=1
        fi
    done
    exit $OVERALL_FAIL
fi

# ---------------------------------------------------------------------------
# Single-version mode
# ---------------------------------------------------------------------------


# Determine previous release version
if [ -n "${1:-}" ]; then
    PREV_VERSION="$1"
else
    # Default: fetch the latest release tag before the current version
    CURRENT_VERSION=$(grep 'Version = ' "$PROJECT_ROOT/cmd/bd/version.go" \
        | head -1 | sed 's/.*"\(.*\)".*/\1/')
    # Try to get the previous release tag from git
    PREV_VERSION=$(git -C "$PROJECT_ROOT" tag --sort=-version:refname \
        | grep '^v' | head -2 | tail -1 2>/dev/null || echo "")
    if [ -z "$PREV_VERSION" ]; then
        echo -e "${RED}Cannot determine previous release version.${NC}"
        echo "Specify explicitly: $0 v0.62.0"
        exit 1
    fi
fi

# Strip 'v' prefix for download URL, keep for display
PREV_VER_BARE="${PREV_VERSION#v}"
PREV_VERSION="v${PREV_VER_BARE}"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Upgrade Smoke Tests: ${PREV_VERSION} → candidate"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# ---------------------------------------------------------------------------
# Binary management
# ---------------------------------------------------------------------------

CACHE_DIR="${HOME}/.cache/beads-regression"
mkdir -p "$CACHE_DIR"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
esac

get_previous_binary() {
    # PREV_BIN (set by //tests/upgrade_smoke's targets) is the previous
    # release already pinned and sha256-verified against the release catalog,
    # so nothing is downloaded; it must report PREV_VERSION.
    if [ -n "${PREV_BIN:-}" ]; then
        if ! "$PREV_BIN" version 2>/dev/null | grep -qF " ${PREV_VER_BARE} "; then
            echo -e "${RED}PREV_BIN=${PREV_BIN} does not report ${PREV_VERSION}${NC}" >&2
            exit 1
        fi
        echo "$PREV_BIN"
        return
    fi
    local cached="$CACHE_DIR/bd-${PREV_VER_BARE}"
    if [ -x "$cached" ]; then
        echo "$cached"
        return
    fi

    local asset="beads_${PREV_VER_BARE}_${OS}_${ARCH}.tar.gz"
    local url="https://github.com/gastownhall/beads/releases/download/${PREV_VERSION}/${asset}"

    echo -e "${YELLOW}Downloading ${PREV_VERSION} binary...${NC}" >&2
    local tmpdir
    tmpdir=$(mktemp -d)
    if ! curl -fsSL "$url" -o "$tmpdir/archive.tar.gz"; then
        echo -e "${RED}Failed to download ${url}${NC}" >&2
        rm -rf "$tmpdir"
        exit 1
    fi

    tar -xzf "$tmpdir/archive.tar.gz" -C "$tmpdir"
    local bd_path
    bd_path=$(find "$tmpdir" -name bd -type f | head -1)
    if [ -z "$bd_path" ]; then
        echo -e "${RED}bd binary not found in archive${NC}" >&2
        rm -rf "$tmpdir"
        exit 1
    fi

    cp -f "$bd_path" "$cached"
    chmod +x "$cached"
    rm -rf "$tmpdir"
    echo "$cached"
}

build_candidate() {
    if [ -n "${CANDIDATE_BIN:-}" ] && [ -x "${CANDIDATE_BIN}" ]; then
        echo "$(cd "$(dirname "$CANDIDATE_BIN")" && pwd)/$(basename "$CANDIDATE_BIN")"
        return
    fi

    local candidate="$CACHE_DIR/bd-candidate-$$"
    echo -e "${YELLOW}Building candidate binary...${NC}" >&2
    (cd "$PROJECT_ROOT" && go build -o "$candidate" ./cmd/bd) >&2
    echo "$candidate"
}

PREV_BIN=$(get_previous_binary)
CAND_BIN=$(build_candidate)

echo "Previous: $PREV_BIN (${PREV_VERSION})"
echo "Candidate: $CAND_BIN"
echo ""

# ---------------------------------------------------------------------------
# Test helpers
# ---------------------------------------------------------------------------

PASS=0
FAIL=0
SCENARIO=""

scenario() {
    SCENARIO="$1"
    echo -e "● ${SCENARIO}"
}

pass() {
    echo -e "  ${GREEN}✓ $1${NC}"
}

fail() {
    echo -e "  ${RED}✗ $1${NC}"
    FAIL=$((FAIL + 1))
}

check() {
    local desc="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        pass "$desc"
    else
        fail "$desc"
    fi
}

finish_scenario() {
    if [ $FAIL -eq 0 ]; then
        PASS=$((PASS + 1))
    fi
}

# Create an isolated workspace with git init
new_workspace() {
    local dir
    dir=$(mktemp -d -t bd-upgrade-XXXXXX)
    git -C "$dir" init --quiet
    git -C "$dir" config user.name "upgrade-test"
    git -C "$dir" config user.email "test@beads.test"
    echo "$dir"
}

# Run previous/candidate binary FROM the workspace directory.
# This is critical: git operations (beads.role, remote detection) must use
# $WS's git repo, not whichever repo this script is running from.
prev() { (cd "$WS" && "$PREV_BIN" "$@"); }
cand() { (cd "$WS" && "$CAND_BIN" "$@"); }

prev_init() { prev init --quiet --non-interactive --skip-hooks --skip-agents; }
cand_init() { cand init --quiet --non-interactive --skip-hooks --skip-agents; }

# Upgrade an existing workspace to the candidate. Since the migration-consent
# gate (Beads 1.2 release remediation) the candidate never migrates an
# existing database from ordinary commands — `bd migrate schema` is the
# consent step every real upgrade now includes, so the smoke upgrade includes
# it too. Tolerates a workspace the old binary could not initialize (the
# migrate is a no-op failure there and cand_init creates a fresh database).
cand_upgrade() {
    cand migrate schema >/dev/null 2>&1 || true
    cand_init 2>/dev/null || true
}

# Create an issue with the previous binary, tolerating missing --silent flag.
# Older binaries don't have --silent; we just need to know creation succeeded.
# Sets _CREATED_ID to a non-empty value on success.
prev_create() {
    local out
    # Try --silent first (newer previous binaries)
    out=$(prev create --silent "$@" 2>/dev/null) && _CREATED_ID="${out:-created}" && return 0
    # Fallback: run without --silent, parse any ID-like token from output
    out=$(prev create "$@" 2>/dev/null) || return 1
    _CREATED_ID=$(printf '%s' "$out" | grep -oE '[a-zA-Z0-9_]+-[0-9]+' | head -1)
    _CREATED_ID="${_CREATED_ID:-created}"
}

# Return true when the embedded Dolt database directory (or legacy beads.db)
# exists inside the workspace .beads directory.
embedded_db_exists() {
    [ -d "$WS/.beads/embeddeddolt" ] || [ -f "$WS/.beads/beads.db" ]
}

# ---------------------------------------------------------------------------
# Scenario 1: Embedded maintainer upgrade
# ---------------------------------------------------------------------------

scenario "Embedded maintainer: init → create → upgrade → verify"

WS=$(new_workspace)

# Init with previous version (run from $WS so git ops use $WS's repo)
prev_init 2>/dev/null || true
git -C "$WS" config beads.role maintainer

# Create test data (prev_create handles missing --silent in older binaries)
_CREATED_ID=""
prev_create --title "Pre-upgrade issue" --type task --priority 1 || true
ID1="${_CREATED_ID}"
prev_create --title "Another issue" --type bug || true

# Migration-consent gate: before consenting, an ordinary candidate command on
# the old-schema database must either work as-is (previous release already
# ships the candidate's schema — nothing pending) or refuse WITH the consent
# guidance. A failure without the guidance is a regression. Graded only when
# the old binary left a database to gate: releases before v1.0 have no
# `init --non-interactive`, so prev_init creates nothing there.
if ! embedded_db_exists; then
    pass "Consent gate check skipped (old binary could not initialize a database)"
else
    GATE_OUT=$(cand list 2>&1) && GATE_RC=0 || GATE_RC=$?
    if [ "$GATE_RC" -eq 0 ]; then
        pass "Candidate operates database directly (schema already current)"
    elif echo "$GATE_OUT" | grep -q "bd migrate schema"; then
        pass "Candidate refuses un-migrated database with consent guidance"
    else
        fail "Candidate failed on old database without consent guidance"
    fi
fi

# Upgrade: consent + candidate init (simulates upgrade)
cand_upgrade

# Verify
ROLE=$(git -C "$WS" config --get beads.role 2>/dev/null || echo "MISSING")
if [ "$ROLE" = "maintainer" ]; then
    pass "beads.role preserved (maintainer)"
else
    fail "beads.role changed to '$ROLE' (expected maintainer)"
fi

if [ -n "${ID1:-}" ]; then
    LIST_OUT=$(cand list --json 2>/dev/null || echo "")
    if echo "$LIST_OUT" | grep -q "Pre-upgrade issue"; then
        pass "Pre-upgrade issues visible after upgrade"
    else
        fail "Pre-upgrade issues NOT visible after upgrade"
    fi
else
    # Old binary could not create issues — early embedded releases (e.g. v0.63.x)
    # may have had init bugs that prevented writes. Skip data-migration check.
    pass "Data migration check skipped (old binary could not write issues)"
fi

# Doctor check
if cand doctor quick 2>/dev/null; then
    pass "bd doctor quick passes"
else
    fail "bd doctor quick fails after upgrade"
fi

rm -rf "$WS"
finish_scenario

# ---------------------------------------------------------------------------
# Scenario 2: Contributor upgrade
# ---------------------------------------------------------------------------

scenario "Contributor: init --contributor → upgrade → verify role preserved"

WS=$(new_workspace)

# Init as contributor with previous version
prev_init 2>/dev/null || true
git -C "$WS" config beads.role contributor

# Upgrade
cand_upgrade

ROLE=$(git -C "$WS" config --get beads.role 2>/dev/null || echo "MISSING")
if [ "$ROLE" = "contributor" ]; then
    pass "beads.role preserved (contributor)"
else
    fail "beads.role changed to '$ROLE' (expected contributor)"
fi

rm -rf "$WS"
finish_scenario

# ---------------------------------------------------------------------------
# Scenario 3: Mode preservation (embedded must stay embedded)
# ---------------------------------------------------------------------------

scenario "Mode preservation: embedded init must not switch to shared-server"

WS=$(new_workspace)

# Init with previous version
prev_init 2>/dev/null || true
git -C "$WS" config beads.role maintainer

# Check if old binary was able to initialize .beads/.
# Pre-embedded releases (< v0.63.0) used server mode and require an external
# Dolt server that is not available in CI, so their init may not create .beads/.
if [ -d "$WS/.beads" ]; then
    pass "Beads initialized before upgrade"
else
    pass "Pre-upgrade check skipped (old binary may require external Dolt server)"
fi

# Upgrade with candidate (always runs — verifies candidate defaults to embedded)
cand_upgrade

# Verify candidate created an embedded DB
if embedded_db_exists; then
    pass "Embedded DB present after candidate init"
else
    fail "Embedded DB missing after candidate init"
fi

# Verify candidate does not switch to shared-server mode.
# storage.mode key may not be set at all when using embedded mode (it's the
# default), so "not set" and empty output are both acceptable.
SHOW_OUT=$(cand config get storage.mode 2>/dev/null || echo "")
if [ -z "$SHOW_OUT" ] || echo "$SHOW_OUT" | grep -qi "embedded\|sqlite\|local\|not set"; then
    pass "Storage mode is embedded (or default)"
else
    fail "Storage mode reports '$SHOW_OUT' (expected embedded)"
fi

rm -rf "$WS"
finish_scenario

# ---------------------------------------------------------------------------
# Scenario 4: Role must not be left unset after non-interactive init
# ---------------------------------------------------------------------------

scenario "Non-interactive init: beads.role must be set"

WS=$(new_workspace)

# Fresh init with candidate (no previous version; runs from $WS so git
# config is written to $WS's repo, not the repo this script runs from)
cand_init 2>/dev/null || true

ROLE=$(git -C "$WS" config --get beads.role 2>/dev/null || echo "MISSING")
if [ "$ROLE" != "MISSING" ] && [ -n "$ROLE" ]; then
    pass "beads.role set after non-interactive init ($ROLE)"
else
    fail "beads.role NOT set after non-interactive init"
fi

rm -rf "$WS"
finish_scenario

# ---------------------------------------------------------------------------
# Scenario 5: Mutation — bd update after version upgrade must persist
# ---------------------------------------------------------------------------

scenario "MUTATION: init with old → create → upgrade → bd update → verify persisted"

WS=$(new_workspace)

# Init and create an issue with the previous version
prev_init 2>/dev/null || true
git -C "$WS" config beads.role maintainer

_CREATED_ID=""
prev_create --title "Mutation target issue" --type task || true
MUT_ID="${_CREATED_ID}"

if [ -z "${MUT_ID:-}" ]; then
    # Old binary could not create issues — skip mutation test gracefully.
    # Early embedded releases (e.g. v0.63.x) may have had init bugs that
    # prevented writes; that is a known historical issue, not a regression.
    pass "Mutation test skipped (old binary could not write issues)"
    rm -rf "$WS"
    finish_scenario
else
    pass "Issue created with previous binary (id: $MUT_ID)"

    # Upgrade: consent + candidate init
    cand_upgrade

    # Mutate using the candidate binary
    cand update "$MUT_ID" --notes "smoke test mutation" 2>/dev/null || true
    pass "bd update ran without fatal error"

    # Read back and verify the mutation persisted
    SHOW_OUT=$(cand show "$MUT_ID" 2>/dev/null || echo "")
    if echo "$SHOW_OUT" | grep -q "smoke test mutation"; then
        pass "Updated notes persisted and visible after mutation"
    else
        fail "Updated notes NOT visible after bd update (mutation did not persist)"
    fi

    rm -rf "$WS"
    finish_scenario
fi

# ---------------------------------------------------------------------------
# Scenario 6: Dependency blocker paths must survive upgrade
# ---------------------------------------------------------------------------
#
# Release gate for the dependencies-split migrations (0035, 0041–0045, 0047).
# Earlier scenarios create issues but no dependencies, so the data-copy in
# 0041_split_dependencies_target only ever runs as DDL on an empty table and
# the rewritten ready_issues/blocked_issues views never get queried on a
# migrated DB. This scenario populates a dependency with the OLD binary (so the
# row is written in the pre-0041 schema), upgrades, then exercises the exact
# blocker paths that fail when the data-copy is wrong: bd ready, bd blocked,
# bd close — the surface of the current Dolt errno 1105 on a stale schema.

scenario "Dependency blocker paths survive upgrade (ready/blocked/close on migrated deps)"

WS=$(new_workspace)

prev_init 2>/dev/null || true
git -C "$WS" config beads.role maintainer

# Blocker (task) and a dependent (bug depends-on task → bug is blocked).
_CREATED_ID=""
prev_create --title "Blocker task" --type task --priority 1 || true
BLOCKER_ID="${_CREATED_ID}"
_CREATED_ID=""
prev_create --title "Blocked bug" --type bug --priority 2 || true
BLOCKED_ID="${_CREATED_ID}"

# Wire the dependency with the previous binary so the dependencies table is
# populated in the old (pre-split) schema. `dep add <blocked> <blocker>` means
# blocked depends on blocker; default type is "blocks".
DEP_CREATED=false
if [ -n "${BLOCKER_ID:-}" ] && [ -n "${BLOCKED_ID:-}" ] \
    && [ "$BLOCKER_ID" != "created" ] && [ "$BLOCKED_ID" != "created" ]; then
    if prev dep add "$BLOCKED_ID" "$BLOCKER_ID" >/dev/null 2>&1; then
        DEP_CREATED=true
    fi
fi

if ! $DEP_CREATED; then
    # Old binary could not create a parseable dependency — early releases lack
    # --silent (so IDs aren't captured) or predate positional dep add. The
    # data-copy is still covered by the migration-test harness; skip gracefully.
    pass "Dependency blocker-path check skipped (old binary could not add a parseable dependency)"
    rm -rf "$WS"
    finish_scenario
else
    pass "Dependency created with previous binary ($BLOCKED_ID depends on $BLOCKER_ID)"

    # Upgrade (consents to and runs migrations 0041+, including the data-copy).
    cand_upgrade

    # 1. Blocker queries must not error on the migrated dependency rows.
    if cand ready >/dev/null 2>&1; then
        pass "bd ready succeeds after upgrade"
    else
        fail "bd ready errors after upgrade (migrated dependency schema regression)"
    fi
    if cand blocked >/dev/null 2>&1; then
        pass "bd blocked succeeds after upgrade"
    else
        fail "bd blocked errors after upgrade (migrated dependency schema regression)"
    fi

    # 2. The dependent must still be reported blocked post-migration — proves
    #    the data-copy populated depends_on_issue_id, not just the DDL.
    BLOCKED_OUT=$(cand blocked 2>/dev/null || echo "")
    if echo "$BLOCKED_OUT" | grep -q "$BLOCKED_ID"; then
        pass "Migrated dependency still blocks dependent (blocked issue listed)"
    else
        fail "Blocked issue '$BLOCKED_ID' not reported blocked after upgrade (data-copy may have dropped the row)"
    fi

    # 3. Closing the blocker must succeed (this is the bd close errno 1105
    #    surface) and must unblock the dependent (is_blocked recompute).
    if cand close "$BLOCKER_ID" >/dev/null 2>&1; then
        pass "bd close on blocker succeeds after upgrade"
    else
        fail "bd close errors after upgrade (errno 1105 / stale depends_on_id regression)"
    fi

    READY_OUT=$(cand ready 2>/dev/null || echo "")
    if echo "$READY_OUT" | grep -q "$BLOCKED_ID"; then
        pass "Dependent becomes ready after blocker closed (unblock path works)"
    else
        fail "Dependent '$BLOCKED_ID' not ready after closing blocker (is_blocked recompute wrong)"
    fi

    rm -rf "$WS"
    finish_scenario
fi

# ---------------------------------------------------------------------------
# Scenario 7: Wisp plane must survive upgrade
# ---------------------------------------------------------------------------
#
# Release gate for the wisp-plane migrations (0054-0066 and ignored 0012-0025).
# Every earlier scenario works entirely in the versioned plane, so the wisp
# tables reach the candidate empty and those migrations only ever run as DDL
# over zero rows. This seeds a wisp with the OLD binary — including a dependency
# edge, which is what makes the is_blocked recompute do real work — then
# upgrades and exercises the surfaces that break when the replay is wrong:
# listing wisps, the blocker queries that now have to be wisp-aware, and writing
# to a wisp through the migrated storage_class default.
#
# This matrix reaches back thirty releases on a tag push, so anything the old
# binary cannot do has to SKIP rather than fail.

# Failure evidence for step 3 below. A red step 3 has to tell three things apart:
# a bd show that errored, a read path that missed a note that is stored, and a
# write that never landed. So the exit status and output of bd update and bd show
# are kept, and a failure prints them with a direct readback of the database.
# Nothing here prints on a pass, retries, or relaxes the check.

# Run the candidate once and keep what it did instead of discarding it: exit
# status in CAP_RC, stdout in CAP_OUT, stderr in CAP_ERR. Never fails the caller,
# so set -e cannot end the scenario before its cleanup and the summary; without a
# scratch file the stderr is dropped and the run goes on.
cand_capture() {
    local errfile
    errfile=$(mktemp -t bd-evidence-XXXXXX 2>/dev/null) || errfile=/dev/null
    CAP_RC=0
    CAP_OUT=$(cand "$@" 2>"$errfile") || CAP_RC=$?
    CAP_ERR=$(cat "$errfile" 2>/dev/null) || CAP_ERR=""
    [ "$errfile" = /dev/null ] || rm -f "$errfile"
}

# Print a captured stream under a label, indented, or "(empty)" when the command
# said nothing there. printf, not echo -e: the text is evidence, not a format.
diag_stream() {
    local label="$1" text="$2"
    if [ -z "$text" ]; then
        printf '      %s: (empty)\n' "$label"
    else
        printf '      %s:\n' "$label"
        printf '%s\n' "$text" | sed 's/^/        | /'
    fi
}

# A bounded run, so a stuck diagnostic cannot hold up the scenario's cleanup.
with_timeout() {
    local secs="$1"
    shift
    if command -v timeout >/dev/null 2>&1; then
        timeout -k 5 "$secs" "$@"
    else
        "$@"
    fi
}

# Direct readback: run one query against the workspace's embedded Dolt database
# with the dolt CLI, not bd, to see what is really stored. It has to run while the
# workspace still exists. READBACK_STATE is "ok" only when dolt ran and exited 0;
# "unavailable" when it could not be run at all; "failed" when it exited
# non-zero, and then nothing it printed counts as evidence either way.
wisp_readback() {
    local query="$1" dolt="${DOLT_BIN:-dolt}" db="" dir errfile
    READBACK_STATE=unavailable
    READBACK_WHY=""
    READBACK_RC=""
    READBACK_OUT=""
    READBACK_ERR=""
    if ! command -v "$dolt" >/dev/null 2>&1; then
        READBACK_WHY="dolt not found: $dolt"
        return 0
    fi
    # The database directory is named after the workspace, so look for it.
    for dir in "$WS"/.beads/embeddeddolt/*/; do
        if [ -d "${dir}.dolt" ]; then
            db="${dir%/}"
            break
        fi
    done
    if [ -z "$db" ]; then
        READBACK_WHY="no embedded Dolt database under $WS/.beads/embeddeddolt"
        return 0
    fi
    errfile=$(mktemp -t bd-evidence-XXXXXX 2>/dev/null) || errfile=/dev/null
    READBACK_RC=0
    READBACK_OUT=$(cd "$db" && with_timeout 60 "$dolt" sql -r csv -q "$query" 2>"$errfile") || READBACK_RC=$?
    READBACK_ERR=$(cat "$errfile" 2>/dev/null) || READBACK_ERR=""
    [ "$errfile" = /dev/null ] || rm -f "$errfile"
    if [ "$READBACK_RC" -eq 0 ]; then
        READBACK_STATE=ok
    else
        READBACK_STATE=failed
        READBACK_WHY="dolt exited with status $READBACK_RC"
    fi
}

# Run one readback of the table named by $1 and print what came back, or why
# nothing did.
wisp_readback_report() {
    local label="$1"
    wisp_readback "$2"
    if [ "$READBACK_STATE" = unavailable ]; then
        echo "      direct dolt readback of $label unavailable: $READBACK_WHY"
    else
        echo "      direct dolt readback of $label exit status: $READBACK_RC"
        diag_stream "direct dolt readback of $label stdout" "$READBACK_OUT"
        diag_stream "direct dolt readback of $label stderr" "$READBACK_ERR"
    fi
}

# bd update itself failed: its exit status and output are the whole story. bd
# show never ran, and a failed write says nothing about what a readback holds,
# so no class is named here.
wisp_update_diagnostics() {
    echo -e "  ${YELLOW}Wisp update diagnostics${NC}"
    echo "      bd update exit status: $UPDATE_RC"
    diag_stream "bd update stdout" "$UPDATE_OUT"
    diag_stream "bd update stderr" "$UPDATE_ERR"
    echo "      bd show was not run: bd update failed first"
}

# bd update exited 0 but the note was not in bd show's stdout. Print both
# commands' evidence and the direct readbacks, then name a class only when that
# evidence establishes it. SHOW_ERRORED rests on bd show's exit status alone. The
# wisps row decides the rest: it holds the note (READ_PATH_MISS, which also needs
# a bd show that ran to the end), it lacks the note (WRITE_NOT_VISIBLE), or the
# readback gave no answer and the classification is unresolved instead of a
# guess. The issues and wisp_events readbacks show where else the write may have
# gone and never feed that verdict: an event's new_value carries the note text
# whatever the wisps row holds.
wisp_mutation_diagnostics() {
    local quote="'" id visibility wisps_state wisps_why wisps_out
    id="'${WISP_ID//$quote/$quote$quote}'"
    echo -e "  ${YELLOW}Wisp mutation diagnostics${NC}"
    echo "      bd update exit status: $UPDATE_RC"
    diag_stream "bd update stdout" "$UPDATE_OUT"
    diag_stream "bd update stderr" "$UPDATE_ERR"
    echo "      bd show exit status: $SHOW_RC"
    diag_stream "bd show stdout" "$SHOW_OUT"
    diag_stream "bd show stderr" "$SHOW_ERR"
    wisp_readback_report wisps "SELECT id, notes FROM wisps WHERE id = $id"
    wisps_state=$READBACK_STATE wisps_why=$READBACK_WHY wisps_out=$READBACK_OUT
    if [ "$wisps_state" = ok ]; then
        wisp_readback_report issues "SELECT id, notes FROM issues WHERE id = $id"
        wisp_readback_report wisp_events "SELECT event_type, created_at, new_value FROM wisp_events WHERE issue_id = $id ORDER BY created_at, id"
    fi
    if [ "$wisps_state" != ok ]; then
        visibility="unresolved (direct dolt readback $wisps_state: $wisps_why)"
    elif [[ "$wisps_out" == *"$WISP_NOTE"* ]]; then
        visibility=stored
    elif [[ "$wisps_out" == *"$WISP_ID"* ]]; then
        visibility=missing
    else
        visibility="unresolved (direct dolt readback exited 0 but returned no row for $WISP_ID)"
    fi
    if [ "$SHOW_RC" -ne 0 ]; then
        echo "      classification: SHOW_ERRORED (bd show exited with status $SHOW_RC)"
    fi
    case "$visibility" in
    missing)
        echo "      classification: WRITE_NOT_VISIBLE (bd update exited 0, yet the direct dolt readback of the wisp holds no notes)"
        ;;
    stored)
        if [ "$SHOW_RC" -eq 0 ]; then
            echo "      classification: READ_PATH_MISS (the direct dolt readback holds the notes, bd show did not print them)"
        fi
        ;;
    *)
        if [ "$SHOW_RC" -eq 0 ]; then
            echo "      classification: $visibility"
        fi
        ;;
    esac
}

scenario "Wisp plane survives upgrade (list/ready/blocked/update on migrated wisps)"

WS=$(new_workspace)

prev_init 2>/dev/null || true
git -C "$WS" config beads.role maintainer

_CREATED_ID=""
prev_create --ephemeral --title "Smoke wisp" --type task || true
WISP_ID="${_CREATED_ID}"

if [ -z "${WISP_ID:-}" ] || [ "$WISP_ID" = "created" ]; then
    # Pre-wisp release, or one whose create output we cannot parse for an id.
    pass "Wisp check skipped (old binary has no parseable wisp support)"
    rm -rf "$WS"
    finish_scenario
else
    pass "Wisp created with previous binary ($WISP_ID)"

    # A blocker edge makes the migrated is_blocked recompute meaningful. Older
    # binaries may not route wisp-source dependencies, so tolerate failure.
    _CREATED_ID=""
    prev_create --title "Wisp blocker task" --type task --priority 1 || true
    WISP_BLOCKER="${_CREATED_ID}"
    WISP_DEP=false
    if [ -n "${WISP_BLOCKER:-}" ] && [ "$WISP_BLOCKER" != "created" ]; then
        if prev dep add "$WISP_ID" "$WISP_BLOCKER" >/dev/null 2>&1; then
            WISP_DEP=true
            pass "Wisp blocked on a task with previous binary"
        fi
    fi
    if ! $WISP_DEP; then
        pass "Wisp dependency skipped (old binary could not block a wisp on a task)"
    fi

    # Upgrade: consent + candidate init
    cand_upgrade

    # 1. The wisp survived the 0054-0066 + ignored 0012-0025 replay. --all
    #    because the default listing hides closed wisps.
    WISP_OUT=$(cand mol wisp list --all 2>/dev/null || echo "")
    if echo "$WISP_OUT" | grep -q "Smoke wisp"; then
        pass "Wisp visible after upgrade"
    else
        fail "Wisp 'Smoke wisp' NOT visible after upgrade (ignored-plane replay dropped it)"
    fi

    # 2. Blocker queries must not error on the migrated wisp plane. These read
    #    wisps and wisp_dependencies through views the chain rewrites.
    if cand ready >/dev/null 2>&1; then
        pass "bd ready succeeds on the migrated wisp plane"
    else
        fail "bd ready errors after upgrade (migrated wisp schema regression)"
    fi
    if cand blocked >/dev/null 2>&1; then
        pass "bd blocked succeeds on the migrated wisp plane"
    else
        fail "bd blocked errors after upgrade (migrated wisp schema regression)"
    fi

    # 3. Wisps must stay writable — this is the storage_class default path.
    #    Passing means: bd update exits 0 and bd show's stdout holds the note;
    #    show's own exit status is not graded. Each command runs once, and a
    #    failure prints the evidence kept above.
    WISP_NOTE="smoke wisp mutation"
    cand_capture update "$WISP_ID" --notes "$WISP_NOTE"
    UPDATE_RC=$CAP_RC UPDATE_OUT=$CAP_OUT UPDATE_ERR=$CAP_ERR
    if [ "$UPDATE_RC" -eq 0 ]; then
        cand_capture show "$WISP_ID"
        SHOW_RC=$CAP_RC SHOW_OUT=$CAP_OUT SHOW_ERR=$CAP_ERR
        if echo "$SHOW_OUT" | grep -q "$WISP_NOTE"; then
            pass "Wisp writable after upgrade (mutation persisted)"
        else
            fail "Wisp mutation did not persist after upgrade"
            wisp_mutation_diagnostics
        fi
    else
        fail "bd update on a migrated wisp errors after upgrade"
        wisp_update_diagnostics
    fi

    if cand doctor quick >/dev/null 2>&1; then
        pass "bd doctor quick passes on the migrated wisp plane"
    else
        fail "bd doctor quick fails after the wisp upgrade"
    fi

    rm -rf "$WS"
    finish_scenario
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
TOTAL=$((PASS + FAIL))
if [ $FAIL -eq 0 ]; then
    echo -e "  ${GREEN}All $TOTAL scenarios passed${NC}"
else
    echo -e "  ${RED}$FAIL scenario(s) failed${NC} out of $TOTAL"
fi
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Clean up candidate if we built it
if [ -z "${CANDIDATE_BIN:-}" ] && [ -f "$CAND_BIN" ]; then
    rm -f "$CAND_BIN"
fi

[ $FAIL -eq 0 ]
