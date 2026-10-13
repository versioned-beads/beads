// Package compare canonicalizes two JSON payloads per RFC 8785 (JCS), hashes
// them, and on mismatch classifies the difference for triage. It uses the same
// canonicalization dependency (github.com/gowebpki/jcs) as
// internal/storage/issueops.
//
// A payload is one JSON object. Payload builds it from an oracle view, and
// CompareViews compares two views on the columns both schemas have, leaving out
// the stamp columns that only ever record who wrote a row and when. Every column
// is null or its exact text, so an integer, decimal or timestamp never passes
// through a float; only the JSON-typed columns are parsed, and their numbers are
// gated by the version-history mint's own number rule (reached through a single
// entry point in internal/storage/issueops) before anything is canonicalized,
// because RFC 8785 would round a number past 2^53 and call two different values
// equal. A pair holding such a number is uncomparable, which is neither a match
// nor a mismatch.
package compare

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/gowebpki/jcs"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

// ErrNotObject reports a payload that is valid JSON but not a JSON object. A
// payload is always one object, so a top-level array, scalar or null is a bug in
// whoever built it, not a pair to compare.
var ErrNotObject = errors.New("payload is not a JSON object")

// CategoryNumberFidelity is the Mismatch.Category of a pair that could not be
// compared because a payload holds a number outside the range a double keeps
// exactly. Claiming a match would be false and claiming a mismatch unproven.
const CategoryNumberFidelity = "number-fidelity"

// CategoryIssueMissing is the Mismatch.Category of a pair where the oracle has a
// row for the issue and the candidate has none: an issue bd lost or never made.
// The candidate lacks the row. The string is stored with every mismatch, so it
// does not change.
const CategoryIssueMissing = "issue-missing"

// CategoryIssueExtra is the Mismatch.Category of a pair where the candidate has a
// row for the issue and the oracle has none: an issue bd holds that the oracle
// does not, such as a delete that did not land. The oracle lacks the row. The
// string is stored with every mismatch, so it does not change.
const CategoryIssueExtra = "issue-extra"

// Result is COMMIT_REPLAY_RESULT: the outcome of comparing an oracle
// payload against a candidate payload.
type Result struct {
	Matched       bool      `json:"matched"`
	OracleHash    string    `json:"oracle_hash"`
	CandidateHash string    `json:"candidate_hash"`
	Mismatch      *Mismatch `json:"mismatch,omitempty"`
}

// Uncomparable reports whether the pair could not be compared at all: it is not
// Matched, and its Mismatch has the number-fidelity category. No hash is
// computed for such a pair, because canonicalizing it is exactly what would
// have rounded the number.
func (r Result) Uncomparable() bool {
	return r.Mismatch != nil && r.Mismatch.Category == CategoryNumberFidelity
}

// Mismatch is MISMATCH: enough context to triage a non-matching comparison
// without re-deriving it -- category plus both payloads verbatim (raw, not
// canonicalized, so a human or driver sees exactly what was sent).
type Mismatch struct {
	Category     string          `json:"category"`
	ExpectedJSON json.RawMessage `json:"expected_json"`
	ActualJSON   json.RawMessage `json:"actual_json"`
	// Section says where in the issue a version-state mismatch differs; the other
	// categories have none.
	Section string `json:"section,omitempty"`
}

// Canonicalize transforms a JSON payload per RFC 8785 (JCS): object keys
// sorted recursively, numbers in ECMA-262 form. Same library and
// error-wrapping style as canonicalDurableState (version_history.go) and
// DependencyMetadataEqual (dependencies.go) -- this is that same dependency
// applied directly to already-serialized JSON, not a reimplementation.
func Canonicalize(payload []byte) ([]byte, error) {
	canonical, err := jcs.Transform(payload)
	if err != nil {
		return nil, fmt.Errorf("canonicalize (RFC 8785): %w", err)
	}
	return canonical, nil
}

// Hash returns the lowercase-hex SHA-256 digest of canonical.
func Hash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Compare canonicalizes and hashes both payloads; Matched reports whether
// the two hashes agree. On mismatch, Mismatch carries both payloads
// verbatim plus a best-effort category from classifyMismatch.
//
// Each payload must be valid UTF-8 and one JSON object, else Compare returns an
// error: invalid bytes are never replaced with U+FFFD, which would let two
// different byte strings compare equal, and anything but an object is a bug in
// whoever built the payload (ErrNotObject). A number in either payload that is
// outside the exact range is not an error: the pair is uncomparable, with the
// number-fidelity category and both payloads verbatim, and the caller carries on.
func Compare(oracleJSON, candidateJSON []byte) (Result, error) {
	if err := checkObject(oracleJSON); err != nil {
		return Result{}, fmt.Errorf("oracle payload: %w", err)
	}
	if err := checkObject(candidateJSON); err != nil {
		return Result{}, fmt.Errorf("candidate payload: %w", err)
	}

	// The gate runs before canonicalization, which would round the very number it
	// exists to catch. It is the version-history mint's own rule, not a copy.
	if issueops.ReplayRefuseUnrepresentableIntegers(oracleJSON) != nil ||
		issueops.ReplayRefuseUnrepresentableIntegers(candidateJSON) != nil {
		return Result{Mismatch: &Mismatch{
			Category:     CategoryNumberFidelity,
			ExpectedJSON: json.RawMessage(oracleJSON),
			ActualJSON:   json.RawMessage(candidateJSON),
		}}, nil
	}

	oracleCanon, err := Canonicalize(oracleJSON)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize oracle payload: %w", err)
	}
	candidateCanon, err := Canonicalize(candidateJSON)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize candidate payload: %w", err)
	}

	oracleHash := Hash(oracleCanon)
	candidateHash := Hash(candidateCanon)

	result := Result{
		Matched:       oracleHash == candidateHash,
		OracleHash:    oracleHash,
		CandidateHash: candidateHash,
	}
	if !result.Matched {
		category, err := classifyMismatch(oracleJSON, candidateJSON)
		if err != nil {
			return Result{}, fmt.Errorf("classifying the mismatch: %w", err)
		}
		result.Mismatch = &Mismatch{
			Category:     category,
			ExpectedJSON: json.RawMessage(oracleJSON),
			ActualJSON:   json.RawMessage(candidateJSON),
		}
	}
	return result, nil
}

// checkObject says whether payload is one JSON object in valid UTF-8. It never
// converts a number, so a literal no float can hold still reaches the number
// gate instead of failing here.
func checkObject(payload []byte) error {
	if !utf8.Valid(payload) {
		return errors.New("payload is not valid UTF-8")
	}
	var syntax json.RawMessage
	if err := json.Unmarshal(payload, &syntax); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	trimmed := bytes.TrimLeft(payload, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w (it is %s)", ErrNotObject, jsonKind(trimmed))
	}
	return nil
}

// jsonKind names the kind of the JSON value that starts text.
func jsonKind(text []byte) string {
	switch {
	case len(text) == 0:
		return "empty"
	case text[0] == '[':
		return "an array"
	case text[0] == '"':
		return "a string"
	case text[0] == 't' || text[0] == 'f':
		return "a boolean"
	case text[0] == 'n':
		return "null"
	default:
		return "a number"
	}
}

// Field-name synonym sets grounded in the real schema (read directly, not
// guessed): internal/storage/schema/migrations/0001_create_issues.up.sql,
// 0002_create_dependencies.up.sql, and version_history.go's issue_versions
// row shape (epoch, revision, change_actor/change_agent,
// attribution_status). A mismatch payload may be shaped like either a raw
// issues row or an issue_versions row, so each set hedges across both.
var (
	depEdgeKeys = map[string]bool{
		"dependencies": true,
		"depends_on":   true,
		"dep_ids":      true,
		"blocked_by":   true,
		"deps":         true,
	}
	versionCountKeys = map[string]bool{
		"content_hash":  true,
		"version":       true,
		"version_count": true,
		"revision":      true,
	}
	attributionKeys = map[string]bool{
		"created_by":         true,
		"owner":              true,
		"actor":              true,
		"sender":             true,
		"assignee":           true,
		"closed_by_session":  true,
		"change_actor":       true,
		"change_agent":       true,
		"attribution_status": true,
	}
	epochKeys = map[string]bool{
		"epoch": true,
	}
	timestampKeys = map[string]bool{
		"created_at":    true,
		"updated_at":    true,
		"closed_at":     true,
		"last_activity": true,
		"due_at":        true,
		"defer_until":   true,
		"compacted_at":  true,
	}
)

// classifyMismatch returns a best-effort category for a mismatch between
// two JSON object payloads, by comparing top-level keys whose values differ
// (present on only one side counts as differing). A payload's sections are
// top-level keys, so a difference in the dependencies section is exactly a
// difference in the dependencies key. Rule order matters:
// dep-edge and version-count are checked before the as-of-mismatch
// catch-all, so a payload that differs in both a dependency edge and a
// timestamp is still reported as dep-edge, the more actionable signal.
// Falls back to "unclassified" -- deliberately not "no-op-false-positive",
// which would assert caller intent this function has no way to know -- when
// no rule matches; the bead's category list is "e.g.", not a closed enum.
//
// A payload that does not decode is an error, never a quiet "unclassified":
// the classifier must not turn its own failure into an answer.
func classifyMismatch(oracleJSON, candidateJSON []byte) (string, error) {
	var oracle, candidate map[string]any
	if err := json.Unmarshal(oracleJSON, &oracle); err != nil {
		return "", fmt.Errorf("decoding the oracle payload: %w", err)
	}
	if err := json.Unmarshal(candidateJSON, &candidate); err != nil {
		return "", fmt.Errorf("decoding the candidate payload: %w", err)
	}

	differing := map[string]bool{}
	for k, v := range oracle {
		if cv, ok := candidate[k]; !ok || !reflect.DeepEqual(v, cv) {
			differing[k] = true
		}
	}
	for k := range candidate {
		if _, ok := oracle[k]; !ok {
			differing[k] = true
		}
	}

	switch {
	case anyKeyIn(differing, depEdgeKeys):
		return "dep-edge", nil
	case anyKeyIn(differing, versionCountKeys):
		return "version-count", nil
	case anyKeyIn(differing, attributionKeys):
		return "attribution", nil
	case anyKeyIn(differing, epochKeys):
		return "epoch", nil
	case len(differing) > 0 && allKeysIn(differing, timestampKeys):
		return "as-of-mismatch", nil
	default:
		return "unclassified", nil
	}
}

func anyKeyIn(keys, set map[string]bool) bool {
	for k := range keys {
		if set[k] {
			return true
		}
	}
	return false
}

func allKeysIn(keys, set map[string]bool) bool {
	for k := range keys {
		if !set[k] {
			return false
		}
	}
	return true
}

// IsTimestampKey reports whether name is one of the wall-clock columns whose
// differences alone classify a mismatch as "as-of-mismatch". Callers that need
// the same set, for example to leave those columns out of a comparison, ask
// here rather than keeping a second list.
func IsTimestampKey(name string) bool {
	return timestampKeys[name]
}
