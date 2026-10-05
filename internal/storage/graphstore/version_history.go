package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ResourceKind names which retained plane holds a subject's version history.
// It is a local read-layer classification of the catalog's immutable backing,
// not a public BDP Type, a Type URL, or a wire enumeration. Two backings
// ("informational" and "dependency") share one kind because both are Links
// whose versions are retained identically; Issues are separated from Memory
// because their ordered history lives on Jim's native issue_versions table
// rather than in graph_preview_versions.
type ResourceKind string

const (
	KindIssue  ResourceKind = "issue"
	KindMemory ResourceKind = "memory"
	KindLink   ResourceKind = "link"
)

// VersionRow is one ordered version of a graph Resource, newest first.
//
// Ordinal is the ordering authority and is local to this store: two clones can
// both hold ordinal 8 for the same subject, each describing a different state
// (see internal/storage/issueops/version_history.go). Version is the opaque
// citable token, the only address a caller may hand back to ReadVersion or
// ReadVersionPair. ChangeAt is for display and time-based selection only; it is
// an observed wall clock, never the ordering key, and nothing here sorts by it.
//
// Removed means "listed, but this token is not citable": the row is a real
// retained version and belongs in the history, yet handing its Version to
// ReadVersion yields ErrGone rather than a Resource. Exactly one row can carry
// it — a deleted Link's final retained version, which is its private deletion
// marker. Nothing is filtered out on that account: a removal is information the
// caller renders, so this flag tells the caller which row to render as a removal
// and to leave out of any "cite this token" guidance. Deleted Memory and
// Issue allocations preserve their final live version without a marker.
//
// The JSON member names are fixed. Ordinal is deliberately not spelled
// "revision": graph records already use revision for the opaque token, and a
// native Issue uses it for the row-lock CAS token, so reusing the name here
// would invite a local ordering key to be read as a citable address.
type VersionRow struct {
	Ordinal     int64     `json:"ordinal"`   // ordering authority
	Version     string    `json:"version"`   // opaque citable token (feeds show --version / compare)
	ChangeAt    time.Time `json:"change_at"` // display and --at selection; never the ordering key
	Actor       string    `json:"actor"`
	Attribution string    `json:"attribution"` // native attribution_status for Issues; "" for Memory/Link
	Removed     bool      `json:"removed"`     // listed, but ReadVersion refuses this token with ErrGone
}

// PreviewVersionListLimit bounds this disposable non-paginated reader. Refusing
// is deliberate: silently returning a prefix would make an ordered history look
// complete, which is the one thing a History reader must not do.
const PreviewVersionListLimit = 1000

// versionBounds are the acquisition bounds one Versions call reads under. They
// travel as a parameter rather than as package state on purpose: a test that
// lowered a package-level var would race every other test in this package and
// -race would flag it, and the bound under test is precisely the one that must
// not drift unobserved. rows bounds the returned list; bytes is charged in SQL
// before any row is acquired into Go.
type versionBounds struct {
	rows  int
	bytes uint64
}

// shippedVersionBounds are what every caller of Versions reads under. Tests
// substitute smaller ones so ErrLimitExceeded is reachable without writing a
// thousand real versions, which is the only reason this is not inlined.
func shippedVersionBounds() versionBounds {
	return versionBounds{rows: PreviewVersionListLimit, bytes: PreviewCurrentReadByteLimit}
}

// Versions returns the ordered version list for one Resource path, newest first.
//
// This command has two reachable shapes, not three, and an empty list is not
// among them:
//   - a path with no allocation: ErrNotFound;
//   - an allocated subject: its ordered list, which is never empty.
//
// Creation IS version 1. Every allocation writes its first retained version in
// the same transaction as its catalog row (records.go createInTx, issues.go
// CreateIssue, dependencies.go AddDependency, informational.go
// AddInformationalLink), so an allocated subject with nothing retained means the
// creating version was lost. That is corruption and is refused with
// ErrInvalidStore, not reported as an empty list.
//
// "This plane cannot order" is likewise unreachable on this build, so
// ErrCapabilityUnavailable is never returned. graph_preview_versions carries
// ordinal NOT NULL in the same row as the snapshot it orders, and every
// graph_preview_issue_versions mapping is written in the same transaction as the
// native issue_versions row it names (each graph Issue write path scopes
// issueops.ScopeVersionedHistoryTransaction(tx, true) for itself). A plane that
// has lost its ordering authority has therefore lost retained rows, which is
// corruption and answered as such.
//
// What remains are the refusals: corrupt retained state is ErrInvalidStore and
// an oversized history is ErrLimitExceeded. Neither is ever reported as an empty
// or shortened list. The CLI renders ErrLimitExceeded under its
// capability_unavailable code, which is a collision worth knowing about when
// reading user-facing output; it is not this reader's answer.
//
// It supplies no snapshots, lineage, or diffs; ReadVersion resolves a returned
// token to a record. A deleted subject still has a history, so this reader does
// not refuse one, and it reports exactly what the retained tables hold. One
// listed token is therefore not resolvable as a Resource: a deleted Link's final
// retained version is its private deletion marker, which ReadVersion refuses
// with ErrGone. That row is flagged Removed rather than withheld, because a
// removal is part of the history. A deleted Memory's final live head is a real
// retained Resource (deletedMemoryInTx reads it) and is not flagged. The same
// holds for a deleted Issue's final native retained snapshot.
func (s *Store) Versions(ctx context.Context, path string) (ResourceKind, []VersionRow, error) {
	return s.versionsWithin(ctx, path, shippedVersionBounds())
}

// versionsWithin is Versions under caller-supplied bounds. Only tests pass
// anything but shippedVersionBounds.
func (s *Store) versionsWithin(ctx context.Context, path string, bounds versionBounds) (ResourceKind, []VersionRow, error) {
	if err := validateResourcePath(path); err != nil {
		return "", nil, err
	}
	var kind ResourceKind
	var rows []VersionRow
	err := s.withTx(ctx, false, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		var err error
		kind, rows, err = s.versionsInTx(ctx, tx, path, bounds)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	return kind, rows, nil
}

// versionsInTx uses the caller's authority-checked transaction and validated
// path. Allocation, ordering and retained rows are all read in that single
// snapshot, so a concurrent writer cannot interleave a new head between the
// kind resolution and the list.
func (s *Store) versionsInTx(ctx context.Context, tx *sql.Tx, path string, bounds versionBounds) (ResourceKind, []VersionRow, error) {
	var kind, backing, state, head string
	var typ, key sql.NullString
	// The type_url bound and the NullString are readVersionInTx's, not an
	// invention: an oversized type_url selects as NULL and then fails the
	// !typ.Valid check below, so a listing cannot be tricked into trusting a
	// type it never actually compared.
	err := tx.QueryRowContext(ctx, `SELECT resource_kind,backing,allocation_state,revision,
 CASE WHEN OCTET_LENGTH(type_url)<=? THEN type_url ELSE NULL END,backing_key
 FROM graph_preview_catalog WHERE path=?`, len(s.ScopeURL())+256, path).Scan(&kind, &backing, &state, &head, &typ, &key)
	if errors.Is(err, sql.ErrNoRows) {
		// Retained rows without an allocation are corruption, not an absence.
		// Same refusal readVersionInTx makes: a History reader must not report
		// "never allocated" for a subject whose versions are still on disk.
		var retained int
		if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM graph_preview_versions WHERE path=?) +
 (SELECT COUNT(*) FROM graph_preview_issue_versions WHERE path=?)`, path, path).Scan(&retained); err != nil {
			return "", nil, err
		}
		if retained != 0 {
			return "", nil, fmt.Errorf("%w: retained subject lacks allocation", ErrInvalidStore)
		}
		return "", nil, fmt.Errorf("%w: graph path has no allocation", ErrNotFound)
	}
	if err != nil {
		return "", nil, err
	}
	if !typ.Valid || !authorityID.MatchString(head) || (state != "live" && state != "deleted") ||
		(kind != "bead" && kind != "link") || (kind == "bead") != strings.HasPrefix(path, "beads/") {
		return "", nil, fmt.Errorf("%w: invalid retained subject allocation", ErrInvalidStore)
	}
	// WHERE THE LINE IS DRAWN, stated here because it is the thing a caller is most
	// likely to assume wrongly: a listing validates the CATALOG, the ORDERING (dense
	// ordinals on the preview plane, catalog head present) and each row's token and
	// actor SHAPE. It does not validate retained CONTENT, because it never decodes a
	// snapshot. So content corruption surfaces when a listed token is READ, not when
	// it is listed -- a malformed Link tombstone is listed with Removed set and then
	// refuses ErrInvalidStore instead of ErrGone on the read; a snapshot whose
	// identity disagrees with the catalog is listed and refuses on the read; a Memory
	// version that fails to decode is listed and refuses on the read.
	//
	// This does NOT weaken #5898's three shapes. The promise was never "every listed
	// token resolves"; it was that a refusal is never dressed up as an empty list. A
	// listed token that refuses on read is still a refusal, just a later one.
	// Content-validated listings would mean decoding every row against the
	// acquisition budget, which is a design change rather than a missing check.
	//
	// A deleted subject is validated the way readVersionInTx validates it, so one
	// catalog does not get two different answers about whether an allocation is
	// well formed depending on which reader asked. The VALIDATION is shared; the
	// record construction deliberately is not -- a listing has no use for the
	// Record deletedMemoryInTx builds, and calling it here would pay for decoding
	// a record only to discard it and would import refusals that are specific to
	// reading one version.
	if state == "deleted" {
		if backing == "generic" {
			if err := s.validDeletedMemoryAllocationInTx(ctx, tx, path, kind, typ.String, head, state, backing, key); err != nil {
				return "", nil, err
			}
		} else if backing == "issue" {
			if err := s.validDeletedIssueAllocationInTx(ctx, tx, path, kind, typ.String, head, state, backing, key); err != nil {
				return "", nil, err
			}
		} else if !s.validDeletedLinkAllocation(kind, typ.String, backing, key) {
			return "", nil, fmt.Errorf("%w: unsupported deleted subject", ErrInvalidStore)
		}
	}
	switch {
	case backing == "issue" && kind == "bead":
		// The Issue plane's ordinal is the native revision, so the mapping must
		// name the same Issue the catalog is bound to before it is trusted. The
		// type URL is compared too: backing alone was never enough to establish
		// that this allocation is the kind of thing it claims to be.
		if typ.String != IssueTypeURL(s.ScopeURL()) || !key.Valid || key.String == "" {
			return "", nil, fmt.Errorf("%w: invalid Issue allocation", ErrInvalidStore)
		}
		rows, err := issueVersionsInTx(ctx, tx, path, key.String, head, bounds)
		return KindIssue, rows, err
	case backing == "generic" && kind == "bead" && typ.String == MemoryTypeURL(s.ScopeURL()):
		// A deleted Memory mints no successor version, so its head stays a real
		// retained Resource: there is no deletion marker on this backing and no
		// row to flag (deletedMemoryInTx reads that same head).
		rows, err := previewVersionsInTx(ctx, tx, path, head, "", bounds)
		return KindMemory, rows, err
	case backing == "informational" && kind == "link" && IsInformationalTypeURL(s.ScopeURL(), typ.String),
		backing == "dependency" && kind == "link" && typ.String == DependencyTypeURL(s.ScopeURL()):
		// Every informational Type the binary knows is accepted here, but only
		// one this store has INSTALLED is valid: a legacy four-Type installation
		// never holds the example Types, so an allocation naming one is corrupt
		// state. readVersionInTx makes the same check, so the list and the
		// single read agree about which allocations are well formed.
		if backing == "informational" {
			if err := s.informationalTypeInTx(ctx, tx, typ.String); err != nil {
				return "", nil, err
			}
		}
		// A deleted Link's head IS its deletion marker: link_lifecycle.go and
		// dependency_unlink.go set the catalog revision to the marker's token in
		// the same transaction that retains the marker row, and readVersionInTx
		// refuses that exact token with ErrGone. It is therefore both the row
		// that satisfies the head check and the one row flagged Removed.
		marker := ""
		if state == "deleted" {
			marker = head
		}
		rows, err := previewVersionsInTx(ctx, tx, path, head, marker, bounds)
		return KindLink, rows, err
	}
	return "", nil, fmt.Errorf("%w: unsupported retained allocation", ErrInvalidStore)
}

// issueVersionsInTx orders a graph Issue's history by the native revision the
// preview mapping points at. Those native rows exist in every graph workspace
// even though config versioned-history.enabled defaults to false: each graph
// Issue write path scopes its own transaction with
// issueops.ScopeVersionedHistoryTransaction(tx, true), which forces recording on
// for that transaction alone.
//
// Lost dual writes get one answer here, not two graded by how much was lost.
// Mappings whose native rows are all gone and mappings only some of which join
// have the same root cause — each mapping is written in the same transaction as
// the native row it names — so both are ErrInvalidStore. Reporting the total
// loss as a missing capability would have made the amount lost change the kind
// of answer, and an empty or shortened list is the one thing this reader must
// never produce.
//
// The ORDER BY is load-bearing, not cosmetic: an unordered select over
// graph_preview_issue_versions returns rows in hash order, measured on a real
// graph workspace.
func issueVersionsInTx(ctx context.Context, tx *sql.Tx, path, backingKey, head string, bounds versionBounds) ([]VersionRow, error) {
	var mapped int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM graph_preview_issue_versions WHERE path=?`, path).Scan(&mapped); err != nil {
		return nil, err
	}
	if mapped > bounds.rows {
		return nil, fmt.Errorf("%w: at most %d retained versions can be returned; preview pagination is unavailable", ErrLimitExceeded, bounds.rows)
	}
	if mapped == 0 {
		// Creation is version 1: issues.go writes the first mapping in the same
		// transaction as the catalog row, so no mapping at all means the
		// creating version was lost rather than never written.
		return nil, fmt.Errorf("%w: allocated Issue retains no versions", ErrInvalidStore)
	}
	// change_actor and attribution_status are VARCHAR on the native plane, so
	// the row count above already bounds this acquisition; no blob is selected.
	rows, err := tx.QueryContext(ctx, `SELECT m.issue_id,m.issue_revision,m.version,v.change_at,v.change_actor,v.attribution_status
 FROM graph_preview_issue_versions m JOIN issue_versions v ON v.issue_id=m.issue_id AND v.revision=m.issue_revision
 WHERE m.path=? ORDER BY v.revision DESC`, path)
	if err != nil {
		return nil, err
	}
	result := []VersionRow{}
	for rows.Next() {
		var issueID string
		var row VersionRow
		var actor, status sql.NullString
		if err := rows.Scan(&issueID, &row.Ordinal, &row.Version, &row.ChangeAt, &actor, &status); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if issueID != backingKey {
			return nil, errors.Join(fmt.Errorf("%w: invalid Issue retained mapping", ErrInvalidStore), rows.Close())
		}
		// change_actor is nullable on the native plane; an unrecorded actor is
		// an empty Actor with attribution_status explaining it, never an error.
		row.Actor, row.Attribution = actor.String, status.String
		row.ChangeAt = row.ChangeAt.UTC()
		// Removed is never set here: Issue deletion retains the final live
		// snapshot and does not mint a deletion marker.
		result = append(result, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(result) != mapped {
		return nil, fmt.Errorf("%w: mapped Issue retained body is missing", ErrInvalidStore)
	}
	result, err = checkedVersionOrder(result)
	if err != nil {
		return nil, err
	}
	// The head check is what catches a SHORT history. Deleting one mapping row
	// lowers the mapping count and the join count together, so len(result) !=
	// mapped cannot see it: the result stays complete-looking and strictly
	// descending, just missing its newest version. Same refusal and wording
	// readIssueVersionInTx makes when the head's mapping is absent.
	//
	// No density check belongs here. These ordinals are native revisions this
	// reader does not own: recordIssueMappingInTx maps whatever current_revision
	// then reads, so any native write that advances an Issue's revision without
	// a graph mapping leaves a legitimate gap. Requiring dense 1..N would refuse
	// healthy Issues, which is worse than the gap it would catch.
	//
	// SO THIS PLANE HAS ONE UNDETECTED CORRUPTION, stated plainly rather than
	// left for a reader to infer from the absence of a check: deleting a MIDDLE
	// mapping row keeps the head present and lowers the mapping count and the
	// join count together, so neither guard fires and the list comes back
	// complete-looking with a version missing. The head check closes the
	// newest-row case only. Density is the wrong instrument, not a forgotten
	// one. A correct guard would have to compare the native revisions for this
	// issue_id against the mapped set, which means deciding whether a graph
	// writer may ever advance a revision without minting a mapping -- a
	// question about the write paths, not about this reader. Measured on the
	// four paths the suite exercises, native revisions are in fact dense
	// (logged by the Issue-plane test), but four call sites are evidence, not
	// an invariant graphstore enforces.
	if !listsVersion(result, head) {
		return nil, fmt.Errorf("%w: current Issue retained mapping is missing", ErrInvalidStore)
	}
	return result, nil
}

// previewVersionsInTx orders Memory and Link history by graph_preview_versions'
// own ordinal, which the preview writer allocates (insertPreviewVersionInTx)
// rather than the Issue domain. Every retained row is reported, including a
// deleted Link's deletion marker, which marker names so it can be flagged
// Removed; see Versions.
func previewVersionsInTx(ctx context.Context, tx *sql.Tx, path, head, marker string, bounds versionBounds) ([]VersionRow, error) {
	var retained int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM graph_preview_versions WHERE path=?`, path).Scan(&retained); err != nil {
		return nil, err
	}
	if retained > bounds.rows {
		return nil, fmt.Errorf("%w: at most %d retained versions can be returned; preview pagination is unavailable", ErrLimitExceeded, bounds.rows)
	}
	if retained == 0 {
		// Creation is version 1 here too: createInTx, AddDependency and
		// AddInformationalLink each retain their first version in the same
		// transaction as the catalog row, so an allocated subject with nothing
		// retained has lost that version.
		return nil, fmt.Errorf("%w: allocated subject retains no versions", ErrInvalidStore)
	}
	// actor is LONGBLOB here, so corrupt or oversized rows must be charged in
	// SQL before any of them is acquired into Go. snapshot is never selected:
	// an ordered list is metadata, so a large history costs only its actors.
	// SUM may otherwise surface as floating point; clamp above the only
	// relevant threshold and cast, exactly as checkCurrentReadBytes does.
	var size uint64
	if err := tx.QueryRowContext(ctx, `SELECT CAST(LEAST(COALESCE(SUM(OCTET_LENGTH(actor)+256),0),?) AS UNSIGNED)
 FROM graph_preview_versions WHERE path=?`, bounds.bytes+1, path).Scan(&size); err != nil {
		return nil, err
	}
	if size > bounds.bytes {
		return nil, fmt.Errorf("%w: retained version list exceeds the %d-byte acquisition budget", ErrLimitExceeded, bounds.bytes)
	}
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,version,change_at,actor FROM graph_preview_versions
 WHERE path=? ORDER BY ordinal DESC`, path)
	if err != nil {
		return nil, err
	}
	result := []VersionRow{}
	for rows.Next() {
		var row VersionRow
		if err := rows.Scan(&row.Ordinal, &row.Version, &row.ChangeAt, &row.Actor); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		row.ChangeAt = row.ChangeAt.UTC()
		// The deletion marker is listed, not withheld, but it is not citable:
		// ReadVersion refuses this exact token with ErrGone.
		row.Removed = marker != "" && row.Version == marker
		// Attribution stays empty: this plane records an actor and derives
		// claimed/unknown from it at read time (validVersionAttribution). There
		// is no stored status column to report, and synthesizing one here would
		// claim a native field this plane does not have.
		result = append(result, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	result, err = checkedVersionOrder(result)
	if err != nil {
		return nil, err
	}
	// This plane's ordinals are dense 1..N on a healthy store:
	// insertPreviewVersionInTx allocates MAX(ordinal)+1 under a unique key and
	// production code never DELETEs from graph_preview_versions. A gap is
	// therefore a lost version, and strict descent alone cannot tell a gapped
	// list from a complete one. Requiring Ordinal == N-i also pins the oldest
	// row at 1, so a lost creating version is caught here as well.
	for i, row := range result {
		if row.Ordinal != int64(len(result)-i) {
			return nil, fmt.Errorf("%w: retained version ordinals are not dense", ErrInvalidStore)
		}
	}
	// The dense check cannot see a history truncated at the newest end: delete
	// the newest row of a live Memory and 1..N-1 is still dense. The catalog's
	// head is the authority that can, and it must be listed whatever the
	// allocation state — for a deleted Link the marker is the head and satisfies
	// this, which is why nothing is exempted here. Same refusal and wording
	// readVersionBytes makes when the head's row is absent.
	if !listsVersion(result, head) {
		return nil, fmt.Errorf("%w: current retained version is missing", ErrInvalidStore)
	}
	return result, nil
}

// listsVersion reports whether the returned history contains a token. It scans
// rather than inspecting result[0]: on the Issue plane the ordering authority is
// the native revision rather than the catalog, so "the head is present" is the
// invariant worth holding, and pinning its position would assert something this
// reader does not own.
func listsVersion(rows []VersionRow, version string) bool {
	for _, row := range rows {
		if row.Version == version {
			return true
		}
	}
	return false
}

// checkedVersionOrder refuses a list the caller could not reason about: every
// token must be a well-formed address, every actor valid UTF-8, and the
// ordinals must descend strictly. Both planes allocate ordinals as MAX+1 under
// a unique key, so a repeated or non-positive ordinal is corruption rather than
// a tie this reader may break by some other column.
func checkedVersionOrder(rows []VersionRow) ([]VersionRow, error) {
	previous := int64(0)
	for i, row := range rows {
		if !authorityID.MatchString(row.Version) || !utf8.ValidString(row.Actor) ||
			!utf8.ValidString(row.Attribution) || row.Ordinal < 1 {
			return nil, fmt.Errorf("%w: invalid retained version row", ErrInvalidStore)
		}
		if i > 0 && row.Ordinal >= previous {
			return nil, fmt.Errorf("%w: retained versions are not strictly ordered", ErrInvalidStore)
		}
		previous = row.Ordinal
	}
	return rows, nil
}
