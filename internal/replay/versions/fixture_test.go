package versions

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// b11aStore is a bd project in a throwaway directory, built from this tree's bd.
// The tests drive it with real bd commands and read it back through the
// harness's own libraries, so what they compare is what bd wrote.
type b11aStore struct {
	t       *testing.T
	bin     string
	project string
	data    string
}

// newB11aStore initializes a project. With history set it turns the store's
// versioned history on before anything is written, so every issue it makes
// records versions from the start.
func newB11aStore(t *testing.T, history bool) *b11aStore {
	t.Helper()
	replaytest.Require(t, replaytest.NeedDolt|replaytest.NeedBd)
	replaytest.Isolate(t)
	bin := replaytest.BdBin(t)
	project := replaytest.InitBdProject(t, "b11a", bin)
	s := &b11aStore{t: t, bin: bin, project: project, data: replaytest.DataDir(t, project)}
	if history {
		s.enableHistory()
	}
	return s
}

func (s *b11aStore) enableHistory() {
	s.t.Helper()
	s.bd("config", "set", "versioned-history.enabled", "true")
}

func (s *b11aStore) bd(args ...string) string {
	s.t.Helper()
	return replaytest.RunBd(s.t, s.bin, s.project, args...)
}

// create makes an issue with the given extra flags and returns its id.
func (s *b11aStore) create(title string, extra ...string) string {
	s.t.Helper()
	args := append([]string{"create", title}, extra...)
	return replaytest.JSONID(s.t, s.bd(append(args, "--json")...))
}

func (s *b11aStore) head() string {
	s.t.Helper()
	return replaytest.HeadCommit(s.t, s.data)
}

// mustView reads an issue's rows at ref. It takes the test it runs under as an
// argument so a subtest fails itself, not its parent.
func mustView(t *testing.T, dir, ref, id string) *oracle.View {
	t.Helper()
	view, err := oracle.ReadView(context.Background(), dir, ref, id)
	if err != nil {
		t.Fatalf("reading %s at %s: %v", id, ref, err)
	}
	if view == nil {
		t.Fatalf("%s has no issues row at %s", id, ref)
	}
	return view
}

// mustRecord reads an issue's latest stored version at ref.
func mustRecord(t *testing.T, dir, ref, id string) *Record {
	t.Helper()
	reading, err := Read(context.Background(), dir, ref, id)
	if err != nil {
		t.Fatalf("reading the version of %s at %s: %v", id, ref, err)
	}
	if reading == nil || reading.Record == nil {
		t.Fatalf("%s has no stored version at %s (reading: %+v)", id, ref, reading)
	}
	return reading.Record
}

// b11aDescription holds a quote, a comma, a newline and non-ASCII text, the
// characters a hand-rendered document gets wrong.
const b11aDescription = "desc with \"quote\", comma\nand newline é ü 日本"

// b11aMetadata is a metadata document with every JSON value kind. It holds
// numbers that survive a float, because the number gate refuses the rest.
const b11aMetadata = `{"n": 12345, "f": 1.5, "nested": {"a": [1,2,3], "z": null}, "s": "é\n", "b": true}`

// b11aCase is one issue of a corpus.
type b11aCase struct {
	name string
	id   string
}

// buildB11aCorpus makes issues with real bd commands that between them produce
// every field kind bd can write: text, numbers, times, pointers set and
// cleared, JSON, labels, edges of several types, claims, closes, events. The
// small corpus is the subset the round-trip tests need.
func buildB11aCorpus(t *testing.T, s *b11aStore, small bool) []b11aCase {
	t.Helper()
	var cases []b11aCase
	add := func(name string, id string) string {
		cases = append(cases, b11aCase{name: name, id: id})
		return id
	}

	parent := add("Parent", s.create("Parent epic", "--type", "epic"))
	add("Rich", s.create("Rich", "--type", "bug", "--priority", "0",
		"--description", b11aDescription, "--design", "design text",
		"--acceptance", "accept text", "--notes", "notes text",
		"--external-ref", "gh-9", "--estimate", "30",
		"--due", "2030-01-02", "--defer", "2030-02-03",
		"--metadata", b11aMetadata))
	add("Waiter", s.create("Waiter", "--waits-for", parent, "--waits-for-gate", "any-children"))
	add("Event", s.create("Event", "--type", "event", "--event-category", "patrol.muted",
		"--event-actor", "agent://a", "--event-target", "bead://b", "--event-payload", `{"k": 1}`))
	alpha := add("Alpha", s.create("Alpha", "--id", "aaa-1", "--force"))
	d3 := add("D3", s.create("D3"))
	s.bd("dep", "add", d3, "external:otherproj:cap", "--type", "blocks")
	s.bd("dep", "add", d3, alpha, "--type", "related")
	if small {
		return cases
	}

	add("Child", s.create("Child", "--parent", parent, "--labels", "a,b", "--spec-id", "SPEC-1", "--assignee", "alice"))
	plain := add("Plain", s.create("Plain", "--description", b11aDescription))
	s.bd("label", "add", plain, "lbl1")
	// Nothing else is set, so nothing here needs zeroing: the stored bytes must
	// equal the projection exactly.
	add("Bare", s.create("Bare", "--description", b11aDescription))
	claimed := add("Claimed", s.create("Claimed"))
	s.bd("update", claimed, "--claim")
	toClose := add("ToClose", s.create("ToClose"))
	s.bd("close", toClose, "--reason", `done "ok", really`)
	withSession := add("ClosedWithSession", s.create("ClosedWithSession"))
	s.bd("close", withSession, "--reason", "r", "--session", "sess-1")
	zero := add("Zero", s.create("Zero", "--estimate", "0", "--due", "2031-05-06"))
	s.bd("update", zero, "--due", "")
	cleared := add("Cleared", s.create("Cleared", "--description", "x", "--notes", "n1"))
	s.bd("update", cleared, "--allow-empty-description", "--description", "")
	s.bd("update", cleared, "--clear-notes")
	ext := add("Ext", s.create("Ext", "--external-ref", "gh-1"))
	s.bd("update", ext, "--external-ref", "")
	d1 := add("D1", s.create("D1"))
	d2 := add("D2", s.create("D2"))
	s.bd("dep", "add", d1, d2, "--type", "related")
	s.bd("dep", "add", d1, parent, "--type", "discovered-from")
	s.bd("dep", "add", d2, "external:otherproj:cap", "--type", "blocks")
	meta := add("Meta", s.create("Meta"))
	s.bd("update", meta, "--set-metadata", "k=v")
	add("StorageClass", s.create("SC", "--storage-class", "unversioned"))
	add("Molecule", s.create("Molecule", "--mol-type", "patrol"))
	add("Status", s.create("Status", "--status", "blocked", "--priority", "4"))
	add("Unicode", s.create("Uni", "--external-ref", `ref "é" 日本`))
	return cases
}

// b11aID returns the id of the named issue of a corpus.
func b11aID(t *testing.T, cases []b11aCase, name string) string {
	t.Helper()
	for _, c := range cases {
		if c.name == name {
			return c.id
		}
	}
	t.Fatalf("the corpus has no %s issue", name)
	return ""
}

// b11aRow builds an oracle row from alternating column names and values: a
// string is text, nil is SQL NULL.
func b11aRow(kv ...any) oracle.Row {
	var r oracle.Row
	for i := 0; i < len(kv); i += 2 {
		r.Columns = append(r.Columns, kv[i].(string))
		switch v := kv[i+1].(type) {
		case nil:
			r.Cells = append(r.Cells, doltcli.Cell{Null: true})
		case string:
			r.Cells = append(r.Cells, doltcli.Cell{Text: v})
		default:
			panic("b11aRow: value must be a string or nil")
		}
	}
	return r
}
