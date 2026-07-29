#!/usr/bin/env bash
#
# mol-attach-guard.sh — PreToolUse guard that corrects the molecule-attach
# command instead of letting an agent conclude the TDD gate is broken.
#
# THE TRAP (bead gm-ghp4l): two different commands both spell a flag
# `--attach`, and they mean opposite things.
#
#   gc formula cook <formula> --attach <BEAD-ID>   # attach sub-DAG to an
#                                                  # EXISTING bead   <- correct
#   bd mol pour <proto-id> --attach <PROTO>        # attach another PROTO to a
#                                                  # NEWLY poured mol
#
# An agent that needs "attach the TDD molecule to my bead" reaches for whichever
# it remembers. `bd mol pour --attach <bead-id>` refuses (correctly — pour always
# creates a NEW root and its --attach takes a proto), and the agent reads that
# refusal as "the gate is broken" and falls back to a manual worktree, bypassing
# the isolated per-bead worktree + red-green loop the gate exists to enforce.
#
# On 2026-07-24 that happened ELEVEN times across three rigs (gascity, cairn,
# beads) in about five hours, making the TDD gate effectively decorative, and it
# nearly triggered an unnecessary 5.5GB .beads/dolt topology migration to "fix"
# a gate that already worked.
#
# Prompt text alone cannot close this: agent sessions are ephemeral and respawn
# with whatever prompt shipped, and a session that has already decided the tool
# is broken is not re-reading its prompt. A guard at the point of use reaches the
# agent at the moment it makes the mistake, which is the only moment that counts.
#
# CONTRACT: Claude Code PreToolUse hook. Reads the tool call as JSON on stdin,
# emits a permission decision as JSON on stdout, exits 0. Exit 0 with no output
# means "no opinion" — the call proceeds normally.
#
# FAIL-OPEN: every unexpected condition (no jq, unparseable payload, non-Bash
# tool) allows the command. A guard that blocks work when it malfunctions is
# worse than the bug it guards against.

set -uo pipefail

# jq is the only dependency; without it we cannot inspect the payload. Fail open.
command -v jq >/dev/null 2>&1 || exit 0

payload="$(cat)"
COMMAND="$(printf '%s' "$payload" | jq -r '.tool_input.command // empty' 2>/dev/null)"
[ -n "$COMMAND" ] || exit 0

# Only `bd mol pour` invocations carrying --attach are candidates. Anything else
# — including the legitimate `bd mol pour <proto>` used by the supervisor patrols
# — is none of our business.
printf '%s' "$COMMAND" | grep -qE '(^|[;&|]|\s)bd\s+mol\s+pour\b' || exit 0
printf '%s' "$COMMAND" | grep -qE '\-\-attach(=|\s)' || exit 0

# Extract the --attach value, supporting both `--attach X` and `--attach=X`,
# and tolerating single/double quotes around the value.
attach_val="$(printf '%s' "$COMMAND" \
  | grep -oE '\-\-attach(=|[[:space:]]+)("[^"]*"|'"'"'[^'"'"']*'"'"'|[^[:space:];&|]+)' \
  | head -n1 \
  | sed -E 's/^--attach(=|[[:space:]]+)//; s/^["'"'"']//; s/["'"'"']$//')"
[ -n "$attach_val" ] || exit 0

# A proto/formula name (mol-tdd-build, mol-witness-patrol, ...) is the LEGITIMATE
# argument to pour's --attach. Leave those alone.
case "$attach_val" in
  mol-*) exit 0 ;;
esac

# A bead id looks like <prefix>-<suffix> with an optional .N step suffix:
# gm-ghp4l, ga-iq5ytx, be-cfm3z, crn-u9r, ga-d8oqyt.3. If --attach carries one of
# these, the agent means "attach the molecule to my existing bead" — the exact
# mistake this guard exists to correct.
printf '%s' "$attach_val" | grep -qE '^[a-z]{2,4}-[a-z0-9]+(\.[0-9]+)*$' || exit 0

reason="WRONG TOOL — this is the gm-ghp4l trap, and the molecule gate is NOT broken.

'bd mol pour --attach' takes a PROTO, and pour always creates a NEW root, so it
structurally cannot attach to a bead that already exists. It will refuse. That
refusal is correct behavior, not a broken gate.

To attach the molecule to the EXISTING bead ${attach_val}, use:

    gc formula cook --rig \"\$GC_RIG\" mol-tdd-build --attach ${attach_val}

--rig is required, not decorative: mol-tdd-build ships in the builder role pack,
which is a rig layer, so cook resolves it only when the scope is explicit.

Verify by the printed line, NOT by molecule_id:

    Attached: ${attach_val} -> <sub-dag-root> (root: <workflow-root>)

cook --attach does not set molecule_id on your bead (gc sling writes that field
on a different path), so re-running 'bd show ${attach_val}' and looking for
molecule_id will look like a failure when the attach in fact succeeded. Confirm
the blocking dependency instead, then run 'bd mol current <sub-dag-root>'.

Cook once — a second cook --attach on the same bead creates a DUPLICATE sub-DAG.

If 'gc formula cook --attach' itself fails, THAT is a real defect: mail mayor
with the exact command and error, then run the work loop manually in your own
isolated worktree. Do not build on the shared template branch."

jq -n --arg reason "$reason" '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    permissionDecision: "deny",
    permissionDecisionReason: $reason
  }
}'
exit 0
