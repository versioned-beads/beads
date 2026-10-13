package versions

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// b11aRowsCannotSupply are the keys of a stored durable_state that the rows of a
// store do not hold, so a projection of the rows cannot have them: the lease
// overlay is a clone-local table, is_blocked is derived and can go stale, and
// labels are a table the harness does not replay until B5 lands.
var b11aRowsCannotSupply = []string{"lease_expires_at", "heartbeat_at", "lease_granted_node", "is_blocked", "labels"}

// b11aAgreeForm renders a document the way the mint would after the keys rows
// cannot supply are taken out of it. One more normalization is applied, to both
// sides alike: a dependency's metadata is a JSON document held in a string,
// and the store spells it with MySQL's spacing where dolt prints it compactly,
// so each is canonicalized as the JSON it is.
func b11aAgreeForm(t *testing.T, doc []byte) []byte {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		t.Fatalf("a durable_state document is not a JSON object: %v\n%s", err, doc)
	}
	for _, key := range b11aRowsCannotSupply {
		delete(top, key)
	}
	if raw, ok := top["dependencies"]; ok {
		var deps []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &deps); err != nil {
			t.Fatalf("dependencies is not an array of objects: %v\n%s", err, raw)
		}
		for _, dep := range deps {
			if meta, ok := dep["metadata"]; ok {
				dep["metadata"] = b11aCanonicalMetadata(t, meta)
			}
		}
		fixed, err := json.Marshal(deps)
		if err != nil {
			t.Fatalf("re-marshalling dependencies: %v", err)
		}
		top["dependencies"] = fixed
	}
	out, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("re-marshalling the document: %v", err)
	}
	canon, err := issueops.ReplayCanonicalDurableState(out)
	if err != nil {
		t.Fatalf("canonicalizing the document: %v\n%s", err, out)
	}
	return canon
}

// b11aCanonicalMetadata canonicalizes the JSON a dependency's metadata string
// holds, and leaves a string that is empty or not JSON as it is.
func b11aCanonicalMetadata(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("a dependency's metadata is not a JSON string: %s", raw)
	}
	if text == "" || !json.Valid([]byte(text)) {
		return raw
	}
	canon, err := compare.Canonicalize([]byte(text))
	if err != nil {
		t.Fatalf("canonicalizing dependency metadata %q: %v", text, err)
	}
	quoted, err := json.Marshal(string(canon))
	if err != nil {
		t.Fatalf("quoting dependency metadata: %v", err)
	}
	return quoted
}

func b11aKeys(t *testing.T, doc []byte) map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		t.Fatalf("a durable_state document is not a JSON object: %v\n%s", err, doc)
	}
	return top
}

// TestB11aProjectionAgrees is the proof the whole version axis rests on: for
// issues made by real bd commands, the document built from the rows of the
// issue at a commit is, byte for byte, the durable_state the mint stored for it
// at the same commit, once the few things rows cannot supply are taken out of
// both. It logs the Dolt release it ran against, and runs on the server lane's
// pinned Dolt and on the local lane alike.
func TestB11aProjectionAgrees(t *testing.T) {
	s := newB11aStore(t, true)
	version, _, _ := strings.Cut(strings.TrimSpace(replaytest.RunDolt(t, s.data, "version")), "\n")
	t.Logf("B11a.ProjectionAgrees dolt version: %s", version)

	cases := buildB11aCorpus(t, s, false)
	head := s.head()

	seen := map[string]bool{}
	seenDep := map[string]bool{}
	richMetadata := false

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := mustRecord(t, s.data, head, c.id)
			view := mustView(t, s.data, head, c.id)

			proj, err := cellsToIssue(view)
			if err != nil {
				t.Fatalf("projecting %s: %v", c.id, err)
			}
			doc, err := json.Marshal(proj)
			if err != nil {
				t.Fatalf("marshalling the projection of %s: %v", c.id, err)
			}

			want := b11aAgreeForm(t, rec.DurableState)
			got := b11aAgreeForm(t, doc)
			if !bytes.Equal(got, want) {
				t.Fatalf("the projection of %s differs from its stored durable_state\nprojected: %s\nstored:    %s", c.id, got, want)
			}

			// Project is what the driver calls; it must be the same function.
			exported, err := Project(view)
			if err != nil {
				t.Fatalf("Project(%s): %v", c.id, err)
			}
			exportedDoc, err := json.Marshal(exported)
			if err != nil {
				t.Fatalf("marshalling Project(%s): %v", c.id, err)
			}
			if !bytes.Equal(exportedDoc, doc) {
				t.Errorf("Project and cellsToIssue disagree for %s\nProject:      %s\ncellsToIssue: %s", c.id, exportedDoc, doc)
			}

			// The comparator production uses reaches the same verdict.
			stored, err := rec.Issue()
			if err != nil {
				t.Fatalf("decoding the stored durable_state of %s: %v", c.id, err)
			}
			res, err := compare.CompareDurable(proj, stored)
			if err != nil {
				t.Fatalf("CompareDurable(%s): %v", c.id, err)
			}
			if !res.Matched {
				t.Errorf("CompareDurable(%s) did not match: %+v", c.id, res.Mismatch)
			}

			for key := range b11aKeys(t, rec.DurableState) {
				seen[key] = true
			}
			var deps []map[string]json.RawMessage
			if raw, ok := b11aKeys(t, rec.DurableState)["dependencies"]; ok {
				if err := json.Unmarshal(raw, &deps); err != nil {
					t.Fatalf("decoding the dependencies of %s: %v", c.id, err)
				}
			}
			for _, dep := range deps {
				for key, value := range dep {
					seenDep[key] = true
					if key == "metadata" && string(value) != `"{}"` {
						richMetadata = true
					}
				}
			}
		})
	}

	t.Run("BareIsByteExact", func(t *testing.T) {
		// Nothing was set on this issue beyond what bd create always writes, so
		// nothing needs taking out: the stored bytes are the canonical form of the
		// projection exactly.
		id := b11aID(t, cases, "Bare")
		rec := mustRecord(t, s.data, head, id)
		proj, err := cellsToIssue(mustView(t, s.data, head, id))
		if err != nil {
			t.Fatalf("projecting %s: %v", id, err)
		}
		doc, err := json.Marshal(proj)
		if err != nil {
			t.Fatalf("marshalling the projection: %v", err)
		}
		canon, err := issueops.ReplayCanonicalDurableState(doc)
		if err != nil {
			t.Fatalf("canonicalizing the projection: %v", err)
		}
		if !bytes.Equal(canon, rec.DurableState) {
			t.Fatalf("the stored durable_state is not the canonical projection\nprojected: %s\nstored:    %s", canon, rec.DurableState)
		}
	})

	t.Run("EdgeOrder", func(t *testing.T) {
		// The rows list an issue's edges by target issue with the NULL ones first,
		// so the edge to an external target comes before the edge to a local issue.
		// The mint stores them by depends_on_id. The projection has to put them in
		// the mint's order, or every issue with both kinds of edge mismatches.
		view := mustView(t, s.data, head, b11aID(t, cases, "D3"))
		if len(view.Dependencies) != 2 {
			t.Fatalf("D3 has %d edges, want 2", len(view.Dependencies))
		}
		if first, ok := view.Dependencies[0].Cell("depends_on_issue_id"); !ok || !first.Null {
			t.Fatalf("the premise of this test is that the rows list the external edge first, got %+v", view.Dependencies[0])
		}
		proj, err := cellsToIssue(view)
		if err != nil {
			t.Fatalf("projecting D3: %v", err)
		}
		if len(proj.Dependencies) != 2 {
			t.Fatalf("the projection of D3 has %d edges, want 2", len(proj.Dependencies))
		}
		if proj.Dependencies[0].DependsOnID != "aaa-1" || proj.Dependencies[1].DependsOnID != "external:otherproj:cap" {
			t.Errorf("edges are not in the mint's order: %q then %q", proj.Dependencies[0].DependsOnID, proj.Dependencies[1].DependsOnID)
		}
	})

	t.Run("StoredKeysAreInTheTable", func(t *testing.T) {
		issueKeys := map[string]Field{}
		for _, f := range IssueFields() {
			issueKeys[f.Key] = f
		}
		depKeys := map[string]Field{}
		for _, f := range DependencyFields() {
			depKeys[f.Key] = f
		}
		for key := range seen {
			f, ok := issueKeys[key]
			switch {
			case !ok:
				t.Errorf("durable_state carries %q, which the drift table does not know", key)
			case f.Class == ClassNotInDurableState:
				t.Errorf("durable_state carries %q, which the drift table says it never does", key)
			}
		}
		for key := range seenDep {
			f, ok := depKeys[key]
			switch {
			case !ok:
				t.Errorf("a stored dependency carries %q, which the drift table does not know", key)
			case f.Class == ClassNotInDurableState:
				t.Errorf("a stored dependency carries %q, which the drift table says it never does", key)
			}
		}
	})

	t.Run("CorpusCoverage", func(t *testing.T) {
		// A corpus that never produced a field proves nothing about it.
		required := []string{
			"id", "title", "status", "priority", "issue_type", "created_at", "created_by",
			"updated_at", "description", "design", "acceptance_criteria", "notes",
			"assignee", "spec_id", "labels", "dependencies", "estimated_minutes",
			"due_at", "defer_until", "external_ref", "metadata", "close_reason",
			"closed_at", "closed_by_session", "started_at", "lease_expires_at",
			"heartbeat_at", "storage_class", "event_kind", "actor", "target",
			"payload", "mol_type",
		}
		for _, key := range required {
			if !seen[key] {
				t.Errorf("no issue in the corpus carries %q in its durable_state", key)
			}
		}
		for _, key := range []string{"issue_id", "depends_on_id", "type", "metadata", "created_at", "created_by"} {
			if !seenDep[key] {
				t.Errorf("no dependency in the corpus carries %q", key)
			}
		}
		if !richMetadata {
			t.Error("no dependency in the corpus has metadata other than the empty document")
		}
		var unseen []string
		for _, f := range IssueFields() {
			if f.Class == ClassCompared && len(f.Columns) > 0 && !seen[f.Key] {
				unseen = append(unseen, f.Key)
			}
		}
		sort.Strings(unseen)
		t.Logf("compared keys bd never produced in this corpus (covered by NullVsEmptyRules): %v", unseen)
	})
}

// TestB11aDurableStateRoundTrips holds the stored document to the type it is
// decoded into: what the mint wrote unmarshals into a types.Issue and marshals
// back, through the mint's own canonicalization, to the same bytes. If it
// does not, comparing decoded states would hide a difference the bytes have.
func TestB11aDurableStateRoundTrips(t *testing.T) {
	s := newB11aStore(t, true)
	cases := buildB11aCorpus(t, s, true)
	head := s.head()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := mustRecord(t, s.data, head, c.id)
			issue, err := rec.Issue()
			if err != nil {
				t.Fatalf("decoding the stored durable_state of %s: %v", c.id, err)
			}
			doc, err := json.Marshal(issue)
			if err != nil {
				t.Fatalf("marshalling the decoded issue: %v", err)
			}
			canon, err := issueops.ReplayCanonicalDurableState(doc)
			if err != nil {
				t.Fatalf("canonicalizing the decoded issue: %v", err)
			}
			if !bytes.Equal(canon, rec.DurableState) {
				t.Fatalf("the stored durable_state does not round-trip\nre-rendered: %s\nstored:      %s", canon, rec.DurableState)
			}
		})
	}
}
