package versions

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// b11aVersionsDDL is the issue_versions table the migrations create, column for
// column: the tests that need a database but not a bd build it by hand.
// durable_state is a LONGBLOB that may be NULL.
const b11aVersionsDDL = `CREATE TABLE issue_versions (
	issue_id varchar(255) NOT NULL,
	revision bigint NOT NULL,
	epoch int NOT NULL,
	durable_state longblob,
	change_actor varchar(255),
	change_agent varchar(255),
	change_message text,
	change_at datetime(6) NOT NULL,
	removed_at datetime(6),
	removed_reason varchar(255),
	attribution_status varchar(20) NOT NULL,
	removed_restriction varchar(30),
	PRIMARY KEY (issue_id, revision)
)`

func b11aSQL(t *testing.T, dir, stmt string) {
	t.Helper()
	replaytest.RunDolt(t, dir, "sql", "-q", stmt)
}

// b11aCommit commits everything in the database and returns the new head.
func b11aCommit(t *testing.T, dir, message string) string {
	t.Helper()
	replaytest.RunDolt(t, dir, "add", "-A")
	replaytest.RunDolt(t, dir, "commit", "-m", message)
	return replaytest.HeadCommit(t, dir)
}

// b11aInsertVersion stores state as the durable_state of a revision, as raw
// bytes: UNHEX keeps the SQL text free of anything that needs escaping.
func b11aInsertVersion(t *testing.T, dir, id string, revision int, state []byte) {
	t.Helper()
	b11aSQL(t, dir, fmt.Sprintf(
		"INSERT INTO issue_versions (issue_id, revision, epoch, durable_state, change_actor, attribution_status, change_at) VALUES ('%s', %d, 1, UNHEX('%s'), 'tester', 'claimed', '2026-10-12 10:00:00.123456')",
		id, revision, hex.EncodeToString(state)))
}

// TestB11aBlobRoundTrip holds the read of durable_state to the bytes the store
// holds. The column is a LONGBLOB, and a CSV of one carries quotes, commas and
// newlines as data; the read goes through HEX so none of that can be confused
// with the framing, and bytes that are not text come back exactly.
func TestB11aBlobRoundTrip(t *testing.T) {
	dir := replaytest.NewDoltDB(t, "blob")
	b11aSQL(t, dir, b11aVersionsDDL)

	cases := []struct {
		id    string
		name  string
		state []byte
		// decodes is whether the bytes are an issue Issue() can decode.
		decodes bool
	}{
		{"quote-1", "quote", []byte(`{"id":"quote-1","title":"say \"hi\" and \\ leave"}`), true},
		{"comma-1", "comma", []byte(`{"id":"comma-1","title":"a,b,,c,"}`), true},
		{"newline-1", "newline", []byte("{\"id\":\"newline-1\",\r\n\"title\":\"t\"\n}"), true},
		{"unicode-1", "non-ascii", []byte(`{"id":"unicode-1","title":"é ü 日本 🙂"}`), true},
		{"nul-1", "nul byte", []byte("{\"id\":\"nul-1\",\x00\"title\":\"t\"}"), false},
		{"high-1", "bytes of 0x80 and up", []byte{0x7b, 0x80, 0xff, 0xfe, 0x7d}, false},
	}
	for _, c := range cases {
		b11aInsertVersion(t, dir, c.id, 1, c.state)
	}
	big := []byte(`{"id":"big-1","title":"` + strings.Repeat("x", 100000) + `"}`)
	b11aSQL(t, dir, `INSERT INTO issue_versions (issue_id, revision, epoch, durable_state, change_actor, attribution_status, change_at) VALUES ('big-1', 1, 1, CAST(CONCAT('{"id":"big-1","title":"', REPEAT('x', 100000), '"}') AS BINARY), 'tester', 'claimed', '2026-10-12 10:00:00.123456')`)
	b11aSQL(t, dir, `INSERT INTO issue_versions (issue_id, revision, epoch, durable_state, attribution_status, change_at) VALUES ('null-1', 1, 1, NULL, 'claimed', '2026-10-12 10:00:00')`)
	b11aInsertVersion(t, dir, "empty-1", 1, nil)
	head := b11aCommit(t, dir, "issue_versions fixture")

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := mustRecord(t, dir, head, c.id)
			if !bytes.Equal(rec.DurableState, c.state) {
				t.Fatalf("durable_state did not come back exactly\nwant: %q\ngot:  %q", c.state, rec.DurableState)
			}
			if rec.IssueID != c.id || rec.Revision != 1 || rec.Epoch != 1 {
				t.Errorf("record = %q revision %d epoch %d, want %q revision 1 epoch 1", rec.IssueID, rec.Revision, rec.Epoch, c.id)
			}
			if rec.ChangeActor != "tester" || rec.AttributionStatus != "claimed" {
				t.Errorf("record actor %q attribution %q, want tester and claimed", rec.ChangeActor, rec.AttributionStatus)
			}
			if !strings.HasPrefix(rec.ChangeAt, "2026-10-12 10:00:00") {
				t.Errorf("record change_at = %q, want a time on 2026-10-12 at 10:00:00", rec.ChangeAt)
			}
			issue, err := rec.Issue()
			if c.decodes {
				if err != nil {
					t.Fatalf("Issue(): %v", err)
				}
				if issue.ID != c.id {
					t.Errorf("Issue().ID = %q, want %q", issue.ID, c.id)
				}
				return
			}
			if err == nil {
				t.Fatalf("Issue() decoded bytes that are not an issue: %+v", issue)
			}
		})
	}

	t.Run("a 100000 byte document", func(t *testing.T) {
		rec := mustRecord(t, dir, head, "big-1")
		if !bytes.Equal(rec.DurableState, big) {
			t.Fatalf("the document did not come back exactly: %d bytes, want %d", len(rec.DurableState), len(big))
		}
	})

	t.Run("a NULL durable_state is a record that cannot be decoded", func(t *testing.T) {
		rec := mustRecord(t, dir, head, "null-1")
		if len(rec.DurableState) != 0 {
			t.Errorf("durable_state = %q, want nothing", rec.DurableState)
		}
		if _, err := rec.Issue(); err == nil {
			t.Error("Issue() decoded a NULL durable_state")
		}
	})

	t.Run("an empty durable_state is a record that cannot be decoded", func(t *testing.T) {
		rec := mustRecord(t, dir, head, "empty-1")
		if len(rec.DurableState) != 0 {
			t.Errorf("durable_state = %q, want nothing", rec.DurableState)
		}
		if _, err := rec.Issue(); err == nil {
			t.Error("Issue() decoded an empty durable_state")
		}
	})
}

// TestB11aReadLatestRevision holds Read to the contract the driver relies on:
// it returns the newest revision of an issue as of the ref it is given, by
// number, and nothing at all, without an error, where there is nothing to read.
func TestB11aReadLatestRevision(t *testing.T) {
	dir := replaytest.NewDoltDB(t, "revisions")
	beforeTable := replaytest.HeadCommit(t, dir)
	b11aSQL(t, dir, b11aVersionsDDL)
	b11aInsertVersion(t, dir, "rev-1", 1, []byte(`{"id":"rev-1","title":"first"}`))
	one := b11aCommit(t, dir, "revision 1")
	b11aInsertVersion(t, dir, "rev-1", 2, []byte(`{"id":"rev-1","title":"second"}`))
	two := b11aCommit(t, dir, "revision 2")
	// Revision 10 sorts before revision 9 as text.
	b11aInsertVersion(t, dir, "order-1", 9, []byte(`{"id":"order-1","title":"nine"}`))
	b11aInsertVersion(t, dir, "order-1", 10, []byte(`{"id":"order-1","title":"ten"}`))
	ten := b11aCommit(t, dir, "revisions 9 and 10")

	title := func(t *testing.T, rec *Record) string {
		t.Helper()
		issue, err := rec.Issue()
		if err != nil {
			t.Fatalf("Issue(): %v", err)
		}
		return issue.Title
	}

	t.Run("the newest revision at the ref", func(t *testing.T) {
		rec := mustRecord(t, dir, two, "rev-1")
		if rec.Revision != 2 || title(t, rec) != "second" {
			t.Errorf("revision %d %q, want revision 2 %q", rec.Revision, title(t, rec), "second")
		}
	})
	t.Run("an older ref reads the older revision", func(t *testing.T) {
		rec := mustRecord(t, dir, one, "rev-1")
		if rec.Revision != 1 || title(t, rec) != "first" {
			t.Errorf("revision %d %q, want revision 1 %q", rec.Revision, title(t, rec), "first")
		}
	})
	t.Run("revisions order by number", func(t *testing.T) {
		rec := mustRecord(t, dir, ten, "order-1")
		if rec.Revision != 10 || title(t, rec) != "ten" {
			t.Errorf("revision %d %q, want revision 10 %q", rec.Revision, title(t, rec), "ten")
		}
	})
	t.Run("an issue with no revision reads as nothing", func(t *testing.T) {
		reading, err := Read(context.Background(), dir, ten, "never-1")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if reading == nil {
			t.Fatal("Read returned no reading, want an empty one")
		}
		if reading.Record != nil || reading.Legacy {
			t.Errorf("reading = %+v, want no record and not legacy", reading)
		}
	})
	t.Run("a ref that predates the table reads as nothing", func(t *testing.T) {
		reading, err := Read(context.Background(), dir, beforeTable, "rev-1")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if reading == nil || reading.Record != nil || reading.Legacy {
			t.Errorf("reading = %+v, want an empty one", reading)
		}
	})
	t.Run("a revision committed later is not seen at an older ref", func(t *testing.T) {
		reading, err := Read(context.Background(), dir, one, "order-1")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if reading == nil || reading.Record != nil {
			t.Errorf("reading = %+v, want no record before revisions 9 and 10 were committed", reading)
		}
	})
}

// TestB11aReadRefErrors holds Read to the three outcomes the oracle's reads
// keep apart: a ref that resolves, a ref that does not, and a ref that cannot
// be used at all. Only the second satisfies errors.Is(err, ErrRefUnresolvable).
func TestB11aReadRefErrors(t *testing.T) {
	dir := replaytest.NewDoltDB(t, "refs")
	b11aSQL(t, dir, b11aVersionsDDL)
	b11aInsertVersion(t, dir, "ref-1", 1, []byte(`{"id":"ref-1","title":"t"}`))
	head := b11aCommit(t, dir, "fixture")
	ctx := context.Background()

	t.Run("a ref that does not resolve", func(t *testing.T) {
		_, err := Read(ctx, dir, "no-such-branch", "ref-1")
		if !errors.Is(err, oracle.ErrRefUnresolvable) {
			t.Fatalf("Read at a branch that does not exist: err = %v, want ErrRefUnresolvable", err)
		}
	})
	t.Run("a ref that is not a ref", func(t *testing.T) {
		for _, ref := range []string{"", "bad ref;", "x'; DROP TABLE issue_versions; --"} {
			_, err := Read(ctx, dir, ref, "ref-1")
			if err == nil {
				t.Errorf("Read at %q succeeded", ref)
			} else if errors.Is(err, oracle.ErrRefUnresolvable) {
				t.Errorf("Read at %q reported an unresolvable ref, not an unusable one: %v", ref, err)
			}
		}
	})
	t.Run("an issue id that is not an id", func(t *testing.T) {
		for _, id := range []string{"", "x'; DROP TABLE issue_versions; --", "a b"} {
			if _, err := Read(ctx, dir, head, id); err == nil {
				t.Errorf("Read of the issue %q succeeded", id)
			}
		}
	})
	t.Run("the fence at a ref that does not resolve", func(t *testing.T) {
		_, err := WriteFence(ctx, dir, "no-such-branch")
		if !errors.Is(err, oracle.ErrRefUnresolvable) {
			t.Fatalf("WriteFence at a branch that does not exist: err = %v, want ErrRefUnresolvable", err)
		}
	})
	t.Run("the fence at a ref that is not a ref", func(t *testing.T) {
		if _, err := WriteFence(ctx, dir, "bad ref;"); err == nil {
			t.Error("WriteFence at a ref that is not a ref succeeded")
		}
	})
}

// TestB11aReadLegacy holds Read's report of a legacy record to the rule that
// defines one: an issue that has no revision, whose row has no
// participation_generation, in a store that has the column. Anything else that
// lacks a revision is missing one, not legacy.
func TestB11aReadLegacy(t *testing.T) {
	ctx := context.Background()

	fenced := replaytest.NewDoltDB(t, "fenced")
	b11aSQL(t, fenced, b11aVersionsDDL)
	b11aSQL(t, fenced, "CREATE TABLE issues (id varchar(255) NOT NULL PRIMARY KEY, participation_generation int)")
	b11aSQL(t, fenced, "INSERT INTO issues (id, participation_generation) VALUES ('legacy-1', NULL), ('current-1', 1), ('current-2', 1)")
	b11aInsertVersion(t, fenced, "current-2", 1, []byte(`{"id":"current-2","title":"t"}`))
	fencedHead := b11aCommit(t, fenced, "fenced fixture")

	for _, c := range []struct {
		name       string
		id         string
		wantRecord bool
		wantLegacy bool
	}{
		{"no revision and no generation is legacy", "legacy-1", false, true},
		{"no revision but a generation is missing, not legacy", "current-1", false, false},
		{"a revision is not legacy", "current-2", true, false},
		{"no issues row is not legacy", "absent-1", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			reading, err := Read(ctx, fenced, fencedHead, c.id)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if reading == nil {
				t.Fatal("Read returned no reading")
			}
			if (reading.Record != nil) != c.wantRecord || reading.Legacy != c.wantLegacy {
				t.Errorf("reading = record %v legacy %v, want record %v legacy %v", reading.Record != nil, reading.Legacy, c.wantRecord, c.wantLegacy)
			}
		})
	}

	unfenced := replaytest.NewDoltDB(t, "unfenced")
	b11aSQL(t, unfenced, b11aVersionsDDL)
	b11aSQL(t, unfenced, "CREATE TABLE issues (id varchar(255) NOT NULL PRIMARY KEY)")
	b11aSQL(t, unfenced, "INSERT INTO issues (id) VALUES ('old-1')")
	unfencedHead := b11aCommit(t, unfenced, "unfenced fixture")

	t.Run("without the column nothing is legacy", func(t *testing.T) {
		reading, err := Read(ctx, unfenced, unfencedHead, "old-1")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if reading == nil || reading.Record != nil || reading.Legacy {
			t.Errorf("reading = %+v, want no record and not legacy", reading)
		}
	})

	bare := replaytest.NewDoltDB(t, "bare")
	bareHead := replaytest.HeadCommit(t, bare)
	for _, c := range []struct {
		name string
		dir  string
		ref  string
		want bool
	}{
		{"a database with no issues table has no fence", bare, bareHead, false},
		{"an issues table without participation_generation has no fence", unfenced, unfencedHead, false},
		{"an issues table with participation_generation has the fence", fenced, fencedHead, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := WriteFence(ctx, c.dir, c.ref)
			if err != nil {
				t.Fatalf("WriteFence: %v", err)
			}
			if got != c.want {
				t.Errorf("WriteFence = %v, want %v", got, c.want)
			}
		})
	}
}

// TestB11aReadSeededStore runs the same rule against a real store whose history
// was turned on after it had issues: bd leaves the issues made before as
// legacy records and mints nothing for a change to one, where an issue made
// after is versioned from its creation. A base without the write fence mints a
// first revision for the changed legacy issue, and the test follows whichever
// the store it built has.
func TestB11aReadSeededStore(t *testing.T) {
	s := newB11aStore(t, false)
	seeded := s.create("Made before history was on")
	s.enableHistory()
	s.bd("update", seeded, "--title", "Changed after history was on")
	created := s.create("Made after history was on")
	s.bd("update", created, "--title", "Created, then changed")
	head := s.head()

	fence, err := WriteFence(context.Background(), s.data, head)
	if err != nil {
		t.Fatalf("WriteFence: %v", err)
	}

	t.Run("the issue that was there first", func(t *testing.T) {
		reading, err := Read(context.Background(), s.data, head, seeded)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if reading == nil {
			t.Fatal("Read returned no reading")
		}
		if fence {
			if reading.Record != nil || !reading.Legacy {
				t.Fatalf("with the write fence, the changed legacy issue has record %v legacy %v, want no record and legacy", reading.Record != nil, reading.Legacy)
			}
			return
		}
		if reading.Record == nil || reading.Record.Revision != 1 || reading.Legacy {
			t.Fatalf("without the write fence, the changed issue has %+v, want revision 1 and not legacy", reading)
		}
	})
	t.Run("the issue made after", func(t *testing.T) {
		rec := mustRecord(t, s.data, head, created)
		if rec.Revision != 2 {
			t.Errorf("revision = %d, want 2 (created, then changed)", rec.Revision)
		}
		issue, err := rec.Issue()
		if err != nil {
			t.Fatalf("Issue(): %v", err)
		}
		if issue.Title != "Created, then changed" {
			t.Errorf("title = %q, want the changed title", issue.Title)
		}
		reading, err := Read(context.Background(), s.data, head, created)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if reading.Legacy {
			t.Error("an issue made after history was on reads as legacy")
		}
	})
	t.Run("the fence follows the schema", func(t *testing.T) {
		columns, err := oracle.Columns(context.Background(), s.data, head, "issues")
		if err != nil {
			t.Fatalf("reading the columns of issues: %v", err)
		}
		has := false
		for _, c := range columns {
			if c == "participation_generation" {
				has = true
			}
		}
		if fence != has {
			t.Errorf("WriteFence = %v, but the issues table has participation_generation = %v", fence, has)
		}
	})
}
