package graphstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	graph "github.com/steveyegge/beads/graphops"
	publicops "github.com/steveyegge/beads/issueops"
)

// ErrVersionUnknown means this subject has no retained mapping for the token.
// It does not assert pruning, erasure, replacement, or non-participation.
var ErrVersionUnknown = errors.New("graph preview retained version is unknown")

// PreviewVersionTokenLimit is an input byte budget, not a token spelling rule.
const PreviewVersionTokenLimit = 4096

// ReadVersion resolves an exact retained preview address without joining current
// payloads or current owned Links. It supplies no ordering, lineage, synthesized
// metadata, or complete public History capability. A private deletion token is
// not a retained Link Resource and returns ErrGone. A missing known component
// is ErrInvalidStore; the HTTP History diagnosis/authorization policy is separate.
// Issue fields excluded from Jim's JSON snapshot (including the current row-lock
// token and content hash) remain zero; they cannot be recovered from current state.
func (s *Store) ReadVersion(ctx context.Context, path, version string) (any, error) {
	if err := validateResourcePath(path); err != nil {
		return nil, err
	}
	if err := validateVersionToken(version); err != nil {
		return nil, err
	}
	var result any
	err := s.withTx(ctx, false, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		var err error
		result, err = s.readVersionInTx(ctx, tx, path, version)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func validateVersionToken(version string) error {
	if version == "" || !utf8.ValidString(version) || len(version) > PreviewVersionTokenLimit {
		return fmt.Errorf("%w: revision must be nonempty UTF-8 within %d bytes", graph.ErrValidation, PreviewVersionTokenLimit)
	}
	return nil
}

// readVersionInTx uses the caller's authority-checked transaction and validated
// path/token. It never joins current payloads into retained state.
func (s *Store) readVersionInTx(ctx context.Context, tx *sql.Tx, path, version string) (any, error) {
	var backing, kind, state, head string
	var typ, key sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT backing,resource_kind,allocation_state,revision,
 CASE WHEN OCTET_LENGTH(type_url)<=? THEN type_url ELSE NULL END,backing_key
 FROM graph_preview_catalog WHERE path=?`, len(s.ScopeURL())+256, path).Scan(&backing, &kind, &state, &head, &typ, &key)
	if errors.Is(err, sql.ErrNoRows) {
		var retained int
		if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM graph_preview_versions WHERE path=?) +
 (SELECT COUNT(*) FROM graph_preview_issue_versions WHERE path=?)`, path, path).Scan(&retained); err != nil {
			return nil, err
		}
		if retained != 0 {
			return nil, fmt.Errorf("%w: retained subject lacks allocation", ErrInvalidStore)
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !typ.Valid || !authorityID.MatchString(head) || (state != "live" && state != "deleted") ||
		(kind == "bead") != strings.HasPrefix(path, "beads/") || (kind != "bead" && kind != "link") {
		return nil, fmt.Errorf("%w: invalid retained subject allocation", ErrInvalidStore)
	}
	if state == "deleted" {
		if backing == "generic" {
			memory, err := s.deletedMemoryInTx(ctx, tx, path)
			if err != nil {
				return nil, err
			}
			if version == head {
				return memory, nil
			}
		} else if backing == "issue" {
			if err := s.validDeletedIssueAllocationInTx(ctx, tx, path, kind, typ.String, head, state, backing, key); err != nil {
				return nil, err
			}
		} else if !s.validDeletedLinkAllocation(kind, typ.String, backing, key) {
			return nil, fmt.Errorf("%w: unsupported deleted subject", ErrInvalidStore)
		}
	}
	if backing == "issue" {
		if kind != "bead" || typ.String != IssueTypeURL(s.ScopeURL()) || !key.Valid || key.String == "" {
			return nil, fmt.Errorf("%w: invalid Issue allocation", ErrInvalidStore)
		}
		return s.readIssueVersionInTx(ctx, tx, path, version, head, key.String)
	}
	if !((backing == "generic" && kind == "bead" && typ.String == MemoryTypeURL(s.ScopeURL())) ||
		(backing == "informational" && kind == "link" && IsInformationalTypeURL(s.ScopeURL(), typ.String)) ||
		(backing == "dependency" && kind == "link" && typ.String == DependencyTypeURL(s.ScopeURL()))) {
		return nil, fmt.Errorf("%w: unsupported retained allocation", ErrInvalidStore)
	}
	if backing == "informational" {
		if err := s.informationalTypeInTx(ctx, tx, typ.String); err != nil {
			return nil, err
		}
	}
	raw, actor, err := readVersionBytes(ctx, tx, path, version, head)
	if err != nil {
		return nil, err
	}
	if state == "deleted" && kind == "link" && version == head {
		var tombstone LinkTombstone
		if json.Unmarshal(raw, &tombstone) != nil || !sameVersionJSON(raw, tombstone) ||
			tombstone.ID != s.ScopeURL()+path || tombstone.Type != typ.String || tombstone.Revision != version ||
			tombstone.Version != version || tombstone.State != "deleted" ||
			!authorityID.MatchString(tombstone.PreviousVersion) || tombstone.PreviousVersion == version ||
			!validVersionAttribution(tombstone.Attribution, actor) {
			return nil, fmt.Errorf("%w: invalid retained deletion marker", ErrInvalidStore)
		}
		return nil, ErrGone
	}
	if kind == "link" {
		var link LinkRecord
		if json.Unmarshal(raw, &link) != nil || link.ID != s.ScopeURL()+path || link.Type != typ.String ||
			link.Revision != version || link.Version != version {
			return nil, fmt.Errorf("%w: retained Link identity differs", ErrInvalidStore)
		}
		if err := s.validateVersionLink(ctx, tx, link, raw, actor); err != nil {
			return nil, err
		}
		return link, nil
	}
	return s.decodeMemoryVersion(ctx, tx, path, typ.String, version, raw, actor)
}

func (s *Store) decodeMemoryVersion(ctx context.Context, tx *sql.Tx, path, typ, version string, raw []byte, actor string) (Record, error) {
	var memory Record
	if json.Unmarshal(raw, &memory) != nil || !sameVersionJSON(raw, memory) || memory.ID != s.ScopeURL()+path ||
		memory.Type != typ || memory.Revision != version || memory.Version != version ||
		!utf8.ValidString(memory.Properties.Title) || !utf8.ValidString(memory.Properties.Body) ||
		!validVersionAttribution(memory.Attribution, actor) {
		return Record{}, fmt.Errorf("%w: invalid retained Memory", ErrInvalidStore)
	}
	if err := s.validateVersionOwned(ctx, tx, memory.ID, MemoryTypeURL(s.ScopeURL()), memory.Owned); err != nil {
		return Record{}, err
	}
	return memory, nil
}

func readVersionBytes(ctx context.Context, tx *sql.Tx, path, version, head string) ([]byte, string, error) {
	var size uint64
	err := tx.QueryRowContext(ctx, `SELECT OCTET_LENGTH(snapshot)+OCTET_LENGTH(actor)
 FROM graph_preview_versions WHERE path=? AND version=?`, path, version).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		if version == head {
			return nil, "", fmt.Errorf("%w: current retained version is missing", ErrInvalidStore)
		}
		return nil, "", ErrVersionUnknown
	}
	if err != nil {
		return nil, "", err
	}
	if size > PreviewCurrentReadByteLimit {
		return nil, "", fmt.Errorf("%w: retained version exceeds acquisition budget", ErrLimitExceeded)
	}
	var raw []byte
	var actor string
	err = tx.QueryRowContext(ctx, `SELECT snapshot,actor FROM graph_preview_versions WHERE path=? AND version=?`, path, version).Scan(&raw, &actor)
	return raw, actor, err
}

func sameVersionJSON(raw []byte, value any) bool {
	canonical, err := canonicalJSON(value)
	return err == nil && bytes.Equal(raw, canonical)
}

func validVersionAttribution(value Attribution, actor string) bool {
	at, err := time.Parse(time.RFC3339Nano, value.RecordedAt)
	return err == nil && at.UTC().Format(time.RFC3339Nano) == value.RecordedAt && utf8.ValidString(actor) &&
		value.Actor == actor && ((actor == "" && value.Status == "unknown") || (actor != "" && value.Status == "claimed"))
}

func (s *Store) validateVersionLink(ctx context.Context, tx *sql.Tx, link LinkRecord, raw []byte, actor string) error {
	path := strings.TrimPrefix(link.ID, s.ScopeURL())
	if validateLinkPath(path) != nil || s.ScopeURL()+path != link.ID || !authorityID.MatchString(link.Revision) ||
		link.Version != link.Revision || !sameVersionJSON(raw, link) || !validVersionAttribution(link.Attribution, actor) {
		return fmt.Errorf("%w: invalid retained Link", ErrInvalidStore)
	}
	if IsInformationalTypeURL(s.ScopeURL(), link.Type) {
		if err := s.informationalTypeInTx(ctx, tx, link.Type); err != nil {
			return err
		}
		return s.validateRetainedInformational(link, raw, actor)
	}
	if link.Type != DependencyTypeURL(s.ScopeURL()) || link.Properties == nil || len(link.Properties) != 0 {
		return fmt.Errorf("%w: invalid retained Dependency", ErrInvalidStore)
	}
	for _, endpoint := range []string{link.Source, link.Target} {
		path := strings.TrimPrefix(endpoint, s.ScopeURL())
		if validatePath(path) != nil || s.ScopeURL()+path != endpoint {
			return fmt.Errorf("%w: invalid retained endpoint", ErrInvalidStore)
		}
	}
	return nil
}

func (s *Store) validateVersionOwned(ctx context.Context, tx *sql.Tx, source, typ string, owned []json.RawMessage) error {
	if owned == nil || len(owned) > PreviewOwnedLinkLimit {
		return fmt.Errorf("%w: invalid retained owned set", ErrInvalidStore)
	}
	// typ is the owner's Bead Type for a Memory, which owns only informational
	// Links, and the one owned Link Type for an Issue. The Memory Type is never an
	// owned Link Type, so it must not pass the Link Type equality.
	memoryOwner := typ == MemoryTypeURL(s.ScopeURL())
	previous := ""
	for _, raw := range owned {
		var link LinkRecord
		if json.Unmarshal(raw, &link) != nil || link.Source != source ||
			(memoryOwner && !IsInformationalTypeURL(s.ScopeURL(), link.Type)) || (!memoryOwner && link.Type != typ) ||
			(previous != "" && graph.CompareCodeUnits(previous, link.ID) >= 0) {
			return fmt.Errorf("%w: invalid retained owned membership", ErrInvalidStore)
		}
		if err := s.validateVersionLink(ctx, tx, link, raw, link.Attribution.Actor); err != nil {
			return err
		}
		previous = link.ID
	}
	return nil
}

func (s *Store) readIssueVersionInTx(ctx context.Context, tx *sql.Tx, path, version, head, backingKey string) (IssueRecord, error) {
	var issueID string
	var ordinal int64
	var ownedSize uint64
	err := tx.QueryRowContext(ctx, `SELECT issue_id,issue_revision,OCTET_LENGTH(owned)
 FROM graph_preview_issue_versions WHERE path=? AND version=?`, path, version).Scan(&issueID, &ordinal, &ownedSize)
	if errors.Is(err, sql.ErrNoRows) {
		if version == head {
			return IssueRecord{}, fmt.Errorf("%w: current Issue retained mapping is missing", ErrInvalidStore)
		}
		return IssueRecord{}, ErrVersionUnknown
	}
	if err != nil {
		return IssueRecord{}, err
	}
	if ordinal < 1 || issueID != backingKey {
		return IssueRecord{}, fmt.Errorf("%w: invalid Issue retained mapping", ErrInvalidStore)
	}
	var bodySize uint64
	err = tx.QueryRowContext(ctx, `SELECT OCTET_LENGTH(durable_state)+OCTET_LENGTH(change_actor)+OCTET_LENGTH(attribution_status)
 FROM issue_versions WHERE issue_id=? AND revision=?`, issueID, ordinal).Scan(&bodySize)
	if errors.Is(err, sql.ErrNoRows) {
		return IssueRecord{}, fmt.Errorf("%w: mapped Issue retained body is missing", ErrInvalidStore)
	}
	if err != nil {
		return IssueRecord{}, err
	}
	if ownedSize > PreviewCurrentReadByteLimit || bodySize > PreviewCurrentReadByteLimit-ownedSize {
		return IssueRecord{}, fmt.Errorf("%w: retained Issue exceeds acquisition budget", ErrLimitExceeded)
	}
	var raw, owned []byte
	var actor, status string
	var at time.Time
	err = tx.QueryRowContext(ctx, `SELECT v.durable_state,v.change_actor,v.attribution_status,v.change_at,m.owned
 FROM graph_preview_issue_versions m JOIN issue_versions v ON v.issue_id=m.issue_id AND v.revision=m.issue_revision
 WHERE m.path=? AND m.version=?`, path, version).Scan(&raw, &actor, &status, &at, &owned)
	if err != nil {
		return IssueRecord{}, err
	}
	var issue publicops.Issue
	result := IssueRecord{ID: s.ScopeURL() + path, Type: IssueTypeURL(s.ScopeURL()), Revision: version, Version: version,
		Attribution: Attribution{Actor: actor, Status: status, RecordedAt: at.UTC().Format(time.RFC3339Nano)}}
	if json.Unmarshal(raw, &issue) != nil || !sameVersionJSON(raw, &issue) || issue.ID != issueID ||
		json.Unmarshal(owned, &result.Owned) != nil || !sameVersionJSON(owned, result.Owned) ||
		!validVersionAttribution(result.Attribution, actor) {
		return IssueRecord{}, fmt.Errorf("%w: invalid retained Issue state", ErrInvalidStore)
	}
	if err := s.validateVersionOwned(ctx, tx, result.ID, DependencyTypeURL(s.ScopeURL()), result.Owned); err != nil {
		return IssueRecord{}, err
	}
	// Match the current graph projection without duplicating Jim's domain body.
	issue.Dependencies = nil
	result.Properties = &issue
	return result, nil
}
