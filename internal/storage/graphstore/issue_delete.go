package graphstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// IssueDeleteRequest previews or applies deletion of one durable graph Issue.
// Apply requires an observed graph revision or an explicit unconditional choice.
type IssueDeleteRequest struct {
	Path, Actor, ExpectedRevision string
	Unconditional, Preview        bool
}

// IssueDeleteResult reports the final live Issue; deletion adds no graph version.
type IssueDeleteResult struct {
	Issue   IssueRecord `json:"issue"`
	Preview bool        `json:"preview"`
	Deleted bool        `json:"deleted"`
}

func (s *Store) DeleteIssue(ctx context.Context, request IssueDeleteRequest) (IssueDeleteResult, error) {
	if err := validatePath(request.Path); err != nil {
		return IssueDeleteResult{}, fmt.Errorf("%w: %v", storage.ErrValidation, err)
	}
	if !utf8.ValidString(request.Actor) {
		return IssueDeleteResult{}, fmt.Errorf("%w: actor must be UTF-8", storage.ErrValidation)
	}
	if request.ExpectedRevision != "" {
		if err := validateVersionToken(request.ExpectedRevision); err != nil {
			return IssueDeleteResult{}, err
		}
	}
	var result IssueDeleteResult
	err := s.withTx(ctx, !request.Preview, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		current, err := s.showIssueInTx(ctx, tx, request.Path)
		if err != nil {
			return err
		}
		if err := checkRevisionGuard(request.ExpectedRevision, request.Unconditional, current.Revision, !request.Preview, "Issue"); err != nil {
			return err
		}
		// The native writer removes dependencies. Refuse every live graph Link
		// first, in the same transaction, so that native cleanup cannot cascade
		// through a graph endpoint. Informational and blocking Links both count.
		links, err := s.incidentLinksInTx(ctx, tx, LinksRequest{BeadPath: request.Path, Direction: "both"}, "")
		if err != nil {
			return err
		}
		if len(links) != 0 {
			ids := make([]string, len(links))
			for i, link := range links {
				ids[i] = link.ID
			}
			return fmt.Errorf("%w: %s has incident Links %s; unlink explicitly before retrying", ErrIncidentLinkConstraint, request.Path, strings.Join(ids, ", "))
		}
		result = IssueDeleteResult{Issue: current, Preview: request.Preview}
		if request.Preview {
			return nil
		}
		if err := s.touchCoordination(ctx, tx); err != nil {
			return err
		}
		// The native Issue writer owns its delete journal, lease and other
		// domain cleanup. It does not mint an issue_versions row for deletion.
		unScope := issueops.ScopeVersionedHistoryTransaction(tx, true)
		defer unScope()
		if err := issueops.DeleteIssueInTx(ctx, tx, current.Properties.ID, request.Actor); err != nil {
			return err
		}
		if err := s.afterStage("issue-delete-native"); err != nil {
			return err
		}
		changed, err := tx.ExecContext(ctx, `UPDATE graph_preview_catalog SET allocation_state='deleted' WHERE path=? AND allocation_state='live' AND revision=? AND backing='issue'`, request.Path, current.Revision)
		if err != nil {
			return err
		}
		if n, err := changed.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return fmt.Errorf("%w: Issue deletion lost its allocation", ErrInvalidStore)
		}
		if err := s.afterStage("issue-delete-allocation"); err != nil {
			return err
		}
		if err := s.validateDeletedIssueInTx(ctx, tx, request.Path); err != nil {
			return err
		}
		result.Deleted = true
		return nil
	})
	if err != nil {
		return IssueDeleteResult{}, err
	}
	return result, nil
}

func (s *Store) validDeletedIssueAllocationInTx(ctx context.Context, tx *sql.Tx, path, kind, typ, head, state, backing string, key sql.NullString) error {
	if validatePath(path) != nil || kind != "bead" || typ != IssueTypeURL(s.ScopeURL()) ||
		!authorityID.MatchString(head) || state != "deleted" || backing != "issue" || !key.Valid || key.String == "" {
		return fmt.Errorf("%w: invalid deleted Issue allocation", ErrInvalidStore)
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM issues WHERE id=?) +
	 (SELECT COUNT(*) FROM graph_preview_payloads WHERE path=?) +
	 (SELECT COUNT(*) FROM graph_preview_links WHERE source_path=? OR target_path=?) +
	 (SELECT COUNT(*) FROM dependencies WHERE issue_id=? OR depends_on_issue_id=?)`, key.String, path, path, path, key.String, key.String).Scan(&current); err != nil {
		return err
	}
	if current != 0 {
		return fmt.Errorf("%w: deleted Issue still has current backing or incident Links", ErrInvalidStore)
	}
	return nil
}

func (s *Store) validateDeletedIssueInTx(ctx context.Context, tx *sql.Tx, path string) error {
	var kind, typ, revision, state, backing string
	var key sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT resource_kind,type_url,revision,allocation_state,backing,backing_key FROM graph_preview_catalog WHERE path=?`, path).Scan(&kind, &typ, &revision, &state, &backing, &key); err != nil {
		return fmt.Errorf("%w: missing deleted Issue allocation: %v", ErrInvalidStore, err)
	}
	if err := s.validDeletedIssueAllocationInTx(ctx, tx, path, kind, typ, revision, state, backing, key); err != nil {
		return err
	}
	issue, err := s.readIssueVersionInTx(ctx, tx, path, revision, revision, key.String)
	if err != nil {
		return err
	}
	if len(issue.Owned) != 0 {
		return fmt.Errorf("%w: deleted Issue final state still owns Links", ErrInvalidStore)
	}
	return nil
}
