package versions

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// writeFenceColumn is the column that makes a store refuse to mint a version for a
// change to a row that was there before history was turned on. A store has the
// column and fences those writes, or has not and mints a first version for them.
const writeFenceColumn = "participation_generation"

// Record is one stored version of an issue: a row of issue_versions as of a ref.
type Record struct {
	IssueID  string
	Revision int64
	Epoch    int
	// DurableState is the bytes the store holds, exactly. They are not necessarily
	// an issue: a NULL or an empty column reads as no bytes, and Issue says what
	// the bytes are when they are not.
	DurableState      []byte
	ChangeActor       string
	AttributionStatus string
	// ChangeAt is the text the store prints for the column, microseconds included.
	ChangeAt string
}

// Issue decodes DurableState into the type the mint marshaled it from. A column
// that is NULL, empty, not valid UTF-8, not one JSON object, or not JSON at all is
// an error: a stored version that cannot be read is a finding, never an issue with
// nothing set.
func (r *Record) Issue() (*types.Issue, error) {
	if len(r.DurableState) == 0 {
		return nil, fmt.Errorf("versions: revision %d of %s has no durable_state", r.Revision, r.IssueID)
	}
	if !utf8.Valid(r.DurableState) {
		return nil, fmt.Errorf("versions: the durable_state of revision %d of %s is not valid UTF-8", r.Revision, r.IssueID)
	}
	if trimmed := bytes.TrimLeft(r.DurableState, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("versions: the durable_state of revision %d of %s is not a JSON object", r.Revision, r.IssueID)
	}
	var issue types.Issue
	if err := json.Unmarshal(r.DurableState, &issue); err != nil {
		return nil, fmt.Errorf("versions: decoding the durable_state of revision %d of %s: %w", r.Revision, r.IssueID, err)
	}
	return &issue, nil
}

// Reading is what the store holds for one issue as of a ref.
type Reading struct {
	// Record is the newest stored version, nil when the issue has none at the ref.
	Record *Record
	// Legacy reports that the issue has no stored version because it is a legacy
	// record: its row has no participation_generation, in a store that has the
	// column. The store fences a change to such a row and mints nothing for it, so
	// the missing version is correct and not a finding. It is false whenever there
	// is a Record.
	Legacy bool
}

// recordColumns is the select list of the one query Read makes of issue_versions.
// durable_state is a LONGBLOB, and a CSV of one carries quotes, commas and newlines
// as data, so it is read as HEX and decoded here: the bytes that come back are
// exactly the bytes that were stored, whatever they are.
const recordColumns = "revision, epoch, HEX(durable_state), change_actor, attribution_status, change_at"

// Read returns the newest stored version of issueID as of ref, newest by revision
// number. Where there is nothing to read it returns a Reading with no Record and
// no error: the ref predates the table, or the issue has no version at it. See the
// oracle package for how failures are told apart: a ref that does not resolve
// satisfies errors.Is(err, oracle.ErrRefUnresolvable), and a ref or an issue id
// that cannot be used does not.
//
// ref and issueID are spliced into the `dolt sql -q` argument, which has no
// bind-parameter API, so both are charset-validated first.
func Read(ctx context.Context, dir, ref, issueID string) (*Reading, error) {
	if err := issueops.ValidateRef(ref); err != nil {
		return nil, fmt.Errorf("versions: invalid ref %q: %w", ref, err)
	}
	if err := issueops.ValidateRef(issueID); err != nil {
		return nil, fmt.Errorf("versions: invalid issue id %q: %w", issueID, err)
	}

	_, rows, err := oracle.Select(ctx, dir, ref, "issue_versions", recordColumns,
		"WHERE issue_id = "+doltcli.SQLQuote(issueID)+" ORDER BY revision DESC LIMIT 1")
	if err != nil {
		return nil, fmt.Errorf("versions: reading the newest version of %s: %w", issueID, err)
	}
	if len(rows) > 0 {
		record, err := scanRecord(issueID, rows[0])
		if err != nil {
			return nil, err
		}
		return &Reading{Record: record}, nil
	}

	legacy, err := isLegacy(ctx, dir, ref, issueID)
	if err != nil {
		return nil, err
	}
	return &Reading{Legacy: legacy}, nil
}

func scanRecord(issueID string, row []doltcli.Cell) (*Record, error) {
	if len(row) != 6 {
		return nil, fmt.Errorf("versions: a version of %s has %d columns, want 6", issueID, len(row))
	}
	revision, err := strconv.ParseInt(row[0].Text, 10, 64)
	if row[0].Null || err != nil {
		return nil, fmt.Errorf("versions: a version of %s has the revision %q, which is not a number", issueID, row[0].Text)
	}
	epoch, err := strconv.Atoi(row[1].Text)
	if row[1].Null || err != nil {
		return nil, fmt.Errorf("versions: revision %d of %s has the epoch %q, which is not a number", revision, issueID, row[1].Text)
	}
	var state []byte
	if !row[2].Null {
		state, err = hex.DecodeString(row[2].Text)
		if err != nil {
			return nil, fmt.Errorf("versions: revision %d of %s: the durable_state did not come back as hex: %w", revision, issueID, err)
		}
	}
	return &Record{
		IssueID:           issueID,
		Revision:          revision,
		Epoch:             epoch,
		DurableState:      state,
		ChangeActor:       row[3].Text,
		AttributionStatus: row[4].Text,
		ChangeAt:          row[5].Text,
	}, nil
}

// isLegacy reports whether the row of issueID at ref is a legacy record: the row
// is there, the store has the write fence, and the row was never given a
// participation generation. It is read with every column of the row, because a
// store without the fence has no such column to select, and the answer is then
// that nothing is legacy.
func isLegacy(ctx context.Context, dir, ref, issueID string) (bool, error) {
	header, rows, err := oracle.Select(ctx, dir, ref, "issues", "*", "WHERE id = "+doltcli.SQLQuote(issueID))
	if err != nil {
		return false, fmt.Errorf("versions: reading the row of %s: %w", issueID, err)
	}
	if len(rows) == 0 {
		return false, nil
	}
	generation, ok := oracle.Row{Columns: header, Cells: rows[0]}.Cell(writeFenceColumn)
	return ok && generation.Null, nil
}

// WriteFence reports whether the issues table at ref has the write fence: the
// participation_generation column. A store with it mints no version for a change
// to a row that has no generation, so the missing version of such a row is not a
// finding; a store without it mints a first version for the change. A database with
// no issues table at ref has no fence. A failure to read is told apart as Read's is.
func WriteFence(ctx context.Context, dir, ref string) (bool, error) {
	columns, err := oracle.Columns(ctx, dir, ref, "issues")
	if err != nil {
		return false, fmt.Errorf("versions: reading the schema of issues: %w", err)
	}
	for _, column := range columns {
		if column == writeFenceColumn {
			return true, nil
		}
	}
	return false, nil
}
