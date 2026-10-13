package oracle

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// b11aSelectFixture is a throwaway database with one table, committed in phases so
// a read can be taken before the table exists, with two rows, and with a third.
type b11aSelectFixture struct {
	dir   string
	root  string // the commit dolt init made: no tables yet
	two   string // the table and two rows
	three string // a third row as well
}

func newB11aSelectFixture(t *testing.T) b11aSelectFixture {
	t.Helper()
	dir := replaytest.NewDoltDB(t, "selectdb")
	fx := b11aSelectFixture{dir: dir, root: replaytest.HeadCommit(t, dir)}

	replaytest.RunDolt(t, dir, "sql", "-q", "CREATE TABLE notes (id VARCHAR(64) PRIMARY KEY, title VARCHAR(255) NOT NULL, note LONGTEXT)")
	replaytest.RunDolt(t, dir, "sql", "-q", `INSERT INTO notes (id, title, note) VALUES
		('x-1', 'One', NULL),
		('x-2', 'Two', '')`)
	fx.two = b2Commit(t, dir, "notes and two rows")

	replaytest.RunDolt(t, dir, "sql", "-q", "INSERT INTO notes (id, title, note) VALUES ('x-3', 'Three', 'note')")
	fx.three = b2Commit(t, dir, "a third row")
	return fx
}

// b11aCells flattens rows to the text of each cell, with NULL as "<NULL>", so a
// comparison reads the way the fixture is written.
func b11aCells(rows [][]doltcli.Cell) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = make([]string, len(row))
		for j, c := range row {
			out[i][j] = c.Text
			if c.Null {
				out[i][j] = "<NULL>"
			}
		}
	}
	return out
}

// B11a.Select: the caller's select list and tail are the statement, read at the ref
// it names, and what comes back is typed as dolt wrote it.
func TestB11aSelect(t *testing.T) {
	ctx := context.Background()
	fx := newB11aSelectFixture(t)

	t.Run("a select list and a tail pick the columns and the rows", func(t *testing.T) {
		header, rows, err := Select(ctx, fx.dir, fx.three, "notes", "id, note", "WHERE id <> 'x-2' ORDER BY id DESC")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if want := []string{"id", "note"}; !reflect.DeepEqual(header, want) {
			t.Errorf("header = %v, want %v", header, want)
		}
		if want := [][]string{{"x-3", "note"}, {"x-1", "<NULL>"}}; !reflect.DeepEqual(b11aCells(rows), want) {
			t.Errorf("rows = %v, want %v", b11aCells(rows), want)
		}
	})

	t.Run("an empty tail reads every row", func(t *testing.T) {
		header, rows, err := Select(ctx, fx.dir, fx.three, "notes", "*", "")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if want := []string{"id", "title", "note"}; !reflect.DeepEqual(header, want) {
			t.Errorf("header = %v, want %v", header, want)
		}
		if len(rows) != 3 {
			t.Errorf("read %d rows, want 3", len(rows))
		}
	})

	t.Run("the ref picks the state that is read", func(t *testing.T) {
		_, atTwo, err := Select(ctx, fx.dir, fx.two, "notes", "id", "ORDER BY id")
		if err != nil {
			t.Fatalf("Select at the second commit: %v", err)
		}
		_, atThree, err := Select(ctx, fx.dir, fx.three, "notes", "id", "ORDER BY id")
		if err != nil {
			t.Fatalf("Select at the third commit: %v", err)
		}
		if want := [][]string{{"x-1"}, {"x-2"}}; !reflect.DeepEqual(b11aCells(atTwo), want) {
			t.Errorf("rows at the second commit = %v, want %v", b11aCells(atTwo), want)
		}
		if want := [][]string{{"x-1"}, {"x-2"}, {"x-3"}}; !reflect.DeepEqual(b11aCells(atThree), want) {
			t.Errorf("rows at the third commit = %v, want %v", b11aCells(atThree), want)
		}
	})

	t.Run("a NULL and an empty string stay different", func(t *testing.T) {
		_, rows, err := Select(ctx, fx.dir, fx.three, "notes", "note", "WHERE id IN ('x-1', 'x-2') ORDER BY id")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		want := [][]doltcli.Cell{{{Null: true}}, {{Text: ""}}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %#v, want %#v", rows, want)
		}
	})

	t.Run("a select list may compute a column", func(t *testing.T) {
		header, rows, err := Select(ctx, fx.dir, fx.three, "notes", "id, HEX(note) AS note_hex", "WHERE id = 'x-3'")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if want := []string{"id", "note_hex"}; !reflect.DeepEqual(header, want) {
			t.Errorf("header = %v, want %v", header, want)
		}
		if want := [][]string{{"x-3", "6E6F7465"}}; !reflect.DeepEqual(b11aCells(rows), want) {
			t.Errorf("rows = %v, want %v", b11aCells(rows), want)
		}
	})

	t.Run("a table the ref predates gives nothing and is not an error", func(t *testing.T) {
		header, rows, err := Select(ctx, fx.dir, fx.root, "notes", "*", "")
		if err != nil || header != nil || rows != nil {
			t.Errorf("Select before the table existed = (%v, %v, %v), want (nil, nil, nil)", header, rows, err)
		}
	})

	t.Run("a table with no row to pick still has its header", func(t *testing.T) {
		header, rows, err := Select(ctx, fx.dir, fx.three, "notes", "id, title", "WHERE id = 'x-9'")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if want := []string{"id", "title"}; !reflect.DeepEqual(header, want) {
			t.Errorf("header = %v, want %v, which is how a table with no row is told from one that is not there", header, want)
		}
		if len(rows) != 0 {
			t.Errorf("read %d rows, want none", len(rows))
		}
	})

	t.Run("a ref that does not resolve is told apart from the rest", func(t *testing.T) {
		for _, ref := range []string{"no-such-branch", "vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv"} {
			_, _, err := Select(ctx, fx.dir, ref, "notes", "*", "")
			if !errors.Is(err, ErrRefUnresolvable) {
				t.Errorf("Select at %q = %v, want ErrRefUnresolvable", ref, err)
			}
		}
	})

	t.Run("a statement dolt refuses is an error and not an absent table", func(t *testing.T) {
		header, rows, err := Select(ctx, fx.dir, fx.three, "notes", "no_such_column", "")
		if err == nil {
			t.Fatalf("Select of a column the table lacks = (%v, %v, nil), want an error", header, rows)
		}
		if errors.Is(err, ErrRefUnresolvable) {
			t.Errorf("Select of a column the table lacks = %v, which says the ref is gone", err)
		}
		if _, _, err := Select(ctx, fx.dir, fx.three, "notes", "id", "WHERE"); err == nil {
			t.Error("Select with a tail that is not a statement = nil error, want one")
		}
	})

	t.Run("an unsafe ref or table name is refused before dolt runs", func(t *testing.T) {
		cases := []struct{ name, ref, table string }{
			{"a ref with a quote", "x'; DROP TABLE notes; --", "notes"},
			{"a ref with a space", "not a ref", "notes"},
			{"an empty ref", "", "notes"},
			{"a table name with a statement", fx.three, "notes; DROP TABLE notes"},
			{"an upper-case table name", fx.three, "Notes"},
			{"an empty table name", fx.three, ""},
		}
		for _, c := range cases {
			header, rows, err := Select(ctx, fx.dir, c.ref, c.table, "*", "")
			if err == nil || header != nil || rows != nil {
				t.Errorf("%s: Select = (%v, %v, %v), want only an error", c.name, header, rows, err)
			}
			if errors.Is(err, ErrRefUnresolvable) {
				t.Errorf("%s: %v reads as a ref that is gone, but it was never sent to dolt", c.name, err)
			}
		}
		// The table the statements named is still there, with all its rows.
		if _, rows, err := Select(ctx, fx.dir, fx.three, "notes", "id", ""); err != nil || len(rows) != 3 {
			t.Errorf("after the refused names the table reads (%d rows, %v), want 3 rows", len(rows), err)
		}
	})
}
