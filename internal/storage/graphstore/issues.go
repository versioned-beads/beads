package graphstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"
	"unicode/utf8"

	graph "github.com/steveyegge/beads/graphops"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

func validateIssueCreate(request publicops.CreateRequest) error {
	if !utf8.ValidString(request.Actor) {
		return fmt.Errorf("%w: Issue actor must be UTF-8", storage.ErrValidation)
	}
	if err := issueops.ValidatePublicCreateRequest(request); err != nil {
		return err
	}
	if request.ParentID != "" || request.InheritLabelsFromParent || len(request.Dependencies) > 0 || request.WaitsFor != nil || request.ForceIDPrefix || request.IDPrefix != "" {
		return fmt.Errorf("%w: Issue preview does not support composite create options or prefix overrides", storage.ErrValidation)
	}
	i := request.Issue
	// CloneCreateRequest materializes empty relation slices. Their lengths
	// were checked above; preserve that empty representation without admitting
	// any dependency or comment values into this bounded adapter.
	allowed := &types.Issue{ID: i.ID, Title: i.Title, Description: i.Description, Notes: i.Notes, Owner: i.Owner, CreatedBy: i.CreatedBy, Design: i.Design, AcceptanceCriteria: i.AcceptanceCriteria, Assignee: i.Assignee, EstimatedMinutes: i.EstimatedMinutes, ExternalRef: i.ExternalRef, SpecID: i.SpecID, IssueType: i.IssueType, Status: i.Status, Priority: i.Priority, DueAt: i.DueAt, Labels: i.Labels, Dependencies: i.Dependencies, Comments: i.Comments}
	if !reflect.DeepEqual(i, allowed) {
		return fmt.Errorf("%w: Issue preview accepts only ID, title, description, design, acceptance, initial notes, owner, creator, assignee, estimate, external/spec references, classification, status, priority, due date and labels; no relationships, metadata, ephemeral or no-history records", storage.ErrValidation)
	}
	return validateIssueCreateFields(i)
}

// CreateIssue applies the existing Issue domain create and Jim Wordelman's
// retained writer in the graph transaction. Specialized tables are the only
// current authority; issue_versions is the only Issue snapshot store.
func (s *Store) CreateIssue(ctx context.Context, path string, request publicops.CreateRequest) (IssueRecord, error) {
	if err := validatePath(path); err != nil {
		return IssueRecord{}, err
	}
	request = issueops.CloneCreateRequest(request)
	if err := validateIssueCreate(request); err != nil {
		return IssueRecord{}, err
	}
	due, err := normalizeIssueDue(request.Issue.DueAt)
	if err != nil {
		return IssueRecord{}, err
	}
	request.Issue.DueAt = due
	revision, err := freshToken()
	if err != nil {
		return IssueRecord{}, err
	}
	var result IssueRecord
	err = s.withTx(ctx, true, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		if issueops.ResolveInfraTypesInTx(ctx, tx)[string(request.Issue.IssueType.Normalize())] {
			return fmt.Errorf("%w: infrastructure Issue types are not admitted by the graph preview", storage.ErrValidation)
		}
		if err := s.touchCoordination(ctx, tx); err != nil {
			return err
		}
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM graph_preview_catalog WHERE path=?`, path).Scan(&exists)
		if err == nil {
			return ErrAlreadyExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		unScope := issueops.ScopeVersionedHistoryTransaction(tx, true)
		defer unScope()
		created, _, err := issueops.ExecuteCreate(ctx, tx, request)
		if err != nil {
			return err
		}
		// ExecuteCreate already owns the initial retained snapshot. Verify its
		// hydrated values before publishing the graph mapping; a coercion rolls
		// back that snapshot and all ordinary create effects in this transaction.
		if !sameIssueCreateFields(request.Issue, created.Issue) {
			return fmt.Errorf("%w: Issue initial fields cannot be represented exactly by storage", storage.ErrValidation)
		}
		if issueops.IsWisp(created.Issue) {
			return fmt.Errorf("%w: Issue preview requires a durable history-bearing Issue", storage.ErrValidation)
		}
		if err := s.afterStage("issue"); err != nil {
			return err
		}
		var ordinal int64
		if err := tx.QueryRowContext(ctx, `SELECT current_revision FROM issues WHERE id=?`, created.Issue.ID).Scan(&ordinal); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO graph_preview_catalog
            (path,resource_kind,type_url,revision,allocation_state,backing,backing_key)
            VALUES (?,'bead',?,?,'live','issue',?)`, path, IssueTypeURL(s.options.Binding.ScopeURL), revision, created.Issue.ID); err != nil {
			return err
		}
		if err := s.afterStage("issue-catalog"); err != nil {
			return err
		}
		// This is an identity mapping, not a second snapshot. The local ordinal
		// never escapes as the graph Version or as a portable History address.
		if _, err := tx.ExecContext(ctx, `INSERT INTO graph_preview_issue_versions (path,version,issue_id,issue_revision,owned) VALUES (?,?,?,?,'[]')`, path, revision, created.Issue.ID, ordinal); err != nil {
			return err
		}
		if err := s.afterStage("retained"); err != nil {
			return err
		}
		// Initial notes have no admitted replacement/clear inverse. Charge the
		// completed mapping and native retained head before committing.
		if request.Issue.Notes != "" {
			if err := checkCurrentReadBytes(ctx, tx); err != nil {
				return err
			}
		}
		result, err = s.showIssueInTx(ctx, tx, path)
		return err
	})
	if err != nil {
		return IssueRecord{}, err
	}
	return result, nil
}

// Read dispatches the immutable backing selection and reads its authoritative
// state in one snapshot. Unsupported resource families remain explicit errors.
func (s *Store) Read(ctx context.Context, path string) (any, error) {
	if err := validateResourcePath(path); err != nil {
		return nil, err
	}
	var result any
	err := s.withTx(ctx, false, func(tx *sql.Tx) error {
		if err := checkCurrentReadBytes(ctx, tx); err != nil {
			return err
		}
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		var backing string
		if err := tx.QueryRowContext(ctx, `SELECT backing FROM graph_preview_catalog WHERE path=?`, path).Scan(&backing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var err error
		switch backing {
		case "generic":
			result, err = s.showMemoryInTx(ctx, tx, path)
		case "issue":
			result, err = s.showIssueInTx(ctx, tx, path)
		case "dependency", "informational":
			result, err = s.showLinkInTx(ctx, tx, path)
		default:
			err = fmt.Errorf("%w: unsupported backing", ErrInvalidStore)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ShowIssue(ctx context.Context, path string) (IssueRecord, error) {
	if err := validatePath(path); err != nil {
		return IssueRecord{}, err
	}
	var result IssueRecord
	err := s.withTx(ctx, false, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		var err error
		result, err = s.showIssueInTx(ctx, tx, path)
		return err
	})
	if err != nil {
		return IssueRecord{}, err
	}
	return result, nil
}

func (s *Store) showIssueInTx(ctx context.Context, tx *sql.Tx, path string) (IssueRecord, error) {
	var kind, typ, revision, state, backing, issueID string
	err := tx.QueryRowContext(ctx, `SELECT resource_kind,type_url,revision,allocation_state,backing,backing_key
        FROM graph_preview_catalog WHERE path=?`, path).Scan(&kind, &typ, &revision, &state, &backing, &issueID)
	if errors.Is(err, sql.ErrNoRows) {
		return IssueRecord{}, ErrNotFound
	}
	if err != nil {
		return IssueRecord{}, err
	}
	if kind != "bead" || typ != IssueTypeURL(s.options.Binding.ScopeURL) || !authorityID.MatchString(revision) || state != "live" || backing != "issue" || issueID == "" {
		return IssueRecord{}, fmt.Errorf("%w: invalid Issue allocation", ErrInvalidStore)
	}
	// Comments are a separate native Issue feed, outside durable_state and its
	// retained graph version. Including them here would make a new comment look
	// like an unversioned mutation of the Issue snapshot.
	issue, err := issueops.HydrateIssueOperationResult(ctx, tx, issueID, false)
	if err != nil {
		return IssueRecord{}, fmt.Errorf("%w: Issue backing: %v", ErrInvalidStore, err)
	}
	var snapshot, retainedOwned []byte
	var actor, status string
	var at time.Time
	var ordinal, current int64
	err = tx.QueryRowContext(ctx, `SELECT m.issue_revision,v.durable_state,v.change_actor,v.attribution_status,v.change_at,i.current_revision,m.owned
        FROM graph_preview_issue_versions m JOIN issue_versions v ON v.issue_id=m.issue_id AND v.revision=m.issue_revision
        JOIN issues i ON i.id=m.issue_id WHERE m.path=? AND m.version=? AND m.issue_id=?`, path, revision, issueID).Scan(&ordinal, &snapshot, &actor, &status, &at, &current, &retainedOwned)
	if err != nil {
		return IssueRecord{}, fmt.Errorf("%w: Issue retained mapping: %v", ErrInvalidStore, err)
	}
	canonical, err := canonicalJSON(issue)
	if err != nil {
		return IssueRecord{}, err
	}
	if current != ordinal || ordinal < 1 || !bytes.Equal(snapshot, canonical) || issueops.IsWisp(issue) || issue.ID != issueID ||
		(actor == "" && status != "unknown") || (actor != "" && status != "claimed") {
		return IssueRecord{}, fmt.Errorf("%w: Issue current/retained state differs or exceeds preview", ErrInvalidStore)
	}
	owned, err := s.ownedLinksInTx(ctx, tx, issueID)
	if err != nil {
		return IssueRecord{}, err
	}
	canonicalOwned, err := canonicalJSON(owned)
	if err != nil {
		return IssueRecord{}, err
	}
	if !bytes.Equal(retainedOwned, canonicalOwned) {
		return IssueRecord{}, fmt.Errorf("%w: Issue owned state differs from retained version", ErrInvalidStore)
	}
	// The private v2 graph projection expresses dependencies once, canonically.
	// Jim's authoritative Issue snapshot still retains its original domain shape.
	issue.Dependencies = nil
	return IssueRecord{ID: graph.CanonicalURL(s.options.Binding.ScopeURL, path), Type: typ, Revision: revision, Version: revision,
		Properties: issue, Owned: owned, Attribution: Attribution{Actor: actor, Status: status, RecordedAt: at.UTC().Format(time.RFC3339Nano)}}, nil
}
