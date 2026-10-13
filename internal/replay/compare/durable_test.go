package compare

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

var b11aWhen = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

// b11aIssue is an issue with two edges, one to an issue and one to an external
// target, in the order the mint stores them.
func b11aIssue() *types.Issue {
	return &types.Issue{
		ID:          "x-1",
		Title:       "T",
		Description: "d",
		Status:      types.StatusOpen,
		Priority:    2,
		IssueType:   types.TypeTask,
		CreatedAt:   b11aWhen,
		UpdatedAt:   b11aWhen,
		Dependencies: []*types.Dependency{
			{IssueID: "x-1", DependsOnID: "aaa-1", Type: types.DepRelated, CreatedAt: b11aWhen, CreatedBy: "alice"},
			{IssueID: "x-1", DependsOnID: "external:p:c", Type: types.DepBlocks, CreatedAt: b11aWhen},
		},
	}
}

func b11aString(s string) *string { return &s }

func b11aTime(h int) *time.Time {
	at := b11aWhen.Add(time.Duration(h) * time.Hour)
	return &at
}

// b11aJSONFields maps each JSON key of a struct type to the index of its field.
func b11aJSONFields(typ reflect.Type) map[string]int {
	fields := map[string]int{}
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = i
	}
	return fields
}

func b11aSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range in {
		out[s] = true
	}
	return out
}

func b11aSortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestB11aStampsIgnored holds the comparator to the line the version axis is
// drawn on: a stamp is a value the store writes about the write, not about the
// issue, and it differs between the two sides by construction, so it is zeroed
// on both before anything is compared. A real field is never ignored.
func TestB11aStampsIgnored(t *testing.T) {
	t.Run("what differs by construction does not mismatch", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			change func(*types.Issue)
		}{
			{"created_at", func(i *types.Issue) { i.CreatedAt = b11aWhen.Add(time.Hour) }},
			{"updated_at", func(i *types.Issue) { i.UpdatedAt = b11aWhen.Add(time.Hour) }},
			{"started_at", func(i *types.Issue) { i.StartedAt = b11aTime(1) }},
			{"closed_at", func(i *types.Issue) { i.ClosedAt = b11aTime(1) }},
			{"closed_by_session", func(i *types.Issue) { i.ClosedBySession = "sess-1" }},
			{"compacted_at", func(i *types.Issue) { i.CompactedAt = b11aTime(1) }},
			{"compacted_at_commit", func(i *types.Issue) { i.CompactedAtCommit = b11aString("abc123") }},
			{"lease_expires_at", func(i *types.Issue) { i.LeaseExpiresAt = b11aTime(1) }},
			{"heartbeat_at", func(i *types.Issue) { i.HeartbeatAt = b11aTime(1) }},
			{"lease_granted_node", func(i *types.Issue) { i.LeaseGrantedNode = "node-1" }},
			{"is_blocked", func(i *types.Issue) { i.IsBlocked = true }},
			{"labels", func(i *types.Issue) { i.Labels = []string{"a", "b"} }},
			{"an edge's created_at", func(i *types.Issue) { i.Dependencies[0].CreatedAt = b11aWhen.Add(time.Hour) }},
			{"an edge's created_by", func(i *types.Issue) { i.Dependencies[1].CreatedBy = "bob" }},
			{"an edge's surrogate id", func(i *types.Issue) { i.Dependencies[0].ID = "00000000-0000-5000-8000-000000000001" }},
		} {
			t.Run(c.name, func(t *testing.T) {
				got := b11aIssue()
				c.change(got)
				res, err := CompareDurable(b11aIssue(), got)
				if err != nil {
					t.Fatalf("CompareDurable: %v", err)
				}
				if !res.Matched {
					t.Fatalf("a difference in %s alone mismatched: %+v", c.name, res.Mismatch)
				}
				if res.OracleHash == "" || res.OracleHash != res.CandidateHash {
					t.Errorf("a match carries equal, non-empty hashes; got %q and %q", res.OracleHash, res.CandidateHash)
				}
			})
		}
	})

	t.Run("every stamp at once is still a match", func(t *testing.T) {
		got := b11aIssue()
		got.CreatedAt, got.UpdatedAt = b11aWhen.Add(time.Hour), b11aWhen.Add(2*time.Hour)
		got.StartedAt, got.ClosedAt, got.CompactedAt = b11aTime(1), b11aTime(2), b11aTime(3)
		got.ClosedBySession, got.CompactedAtCommit = "sess-1", b11aString("abc")
		got.LeaseExpiresAt, got.HeartbeatAt, got.LeaseGrantedNode = b11aTime(4), b11aTime(5), "node-1"
		got.IsBlocked, got.Labels = true, []string{"l"}
		got.Dependencies[0].ID, got.Dependencies[0].CreatedBy = "id-0", "carol"
		res, err := CompareDurable(b11aIssue(), got)
		if err != nil || !res.Matched {
			t.Fatalf("CompareDurable = %+v, %v; want a match", res, err)
		}
	})

	t.Run("a real difference mismatches and names its section", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			change  func(*types.Issue)
			section string
		}{
			{"title", func(i *types.Issue) { i.Title = "other" }, SectionIssue},
			{"description", func(i *types.Issue) { i.Description = "" }, SectionIssue},
			{"priority", func(i *types.Issue) { i.Priority = 0 }, SectionIssue},
			{"status", func(i *types.Issue) { i.Status = types.StatusClosed }, SectionIssue},
			{"an external_ref that is empty rather than NULL", func(i *types.Issue) { i.ExternalRef = b11aString("") }, SectionIssue},
			{"estimated_minutes 0 rather than NULL", func(i *types.Issue) { zero := 0; i.EstimatedMinutes = &zero }, SectionIssue},
			{"metadata", func(i *types.Issue) { i.Metadata = json.RawMessage(`{"k":"v"}`) }, SectionIssue},
			{"an edge's target", func(i *types.Issue) { i.Dependencies[0].DependsOnID = "bbb-2" }, SectionDependencies},
			{"an edge's type", func(i *types.Issue) { i.Dependencies[0].Type = types.DepBlocks }, SectionDependencies},
			{"an edge's metadata", func(i *types.Issue) { i.Dependencies[0].Metadata = "x" }, SectionDependencies},
			{"an edge's thread", func(i *types.Issue) { i.Dependencies[1].ThreadID = "t-1" }, SectionDependencies},
			{"an edge that is missing", func(i *types.Issue) { i.Dependencies = i.Dependencies[:1] }, SectionDependencies},
			{"every edge missing", func(i *types.Issue) { i.Dependencies = nil }, SectionDependencies},
			{"an edge that is extra", func(i *types.Issue) {
				i.Dependencies = append(i.Dependencies, &types.Dependency{IssueID: "x-1", DependsOnID: "zzz-9", Type: types.DepBlocks})
			}, SectionDependencies},
			{"edges in the wrong order", func(i *types.Issue) {
				i.Dependencies[0], i.Dependencies[1] = i.Dependencies[1], i.Dependencies[0]
			}, SectionDependencies},
			{"an issue field and an edge", func(i *types.Issue) {
				i.Title = "other"
				i.Dependencies[0].DependsOnID = "bbb-2"
			}, SectionIssue + "+" + SectionDependencies},
		} {
			t.Run(c.name, func(t *testing.T) {
				got := b11aIssue()
				c.change(got)
				res, err := CompareDurable(b11aIssue(), got)
				if err != nil {
					t.Fatalf("CompareDurable: %v", err)
				}
				if res.Matched || res.Mismatch == nil {
					t.Fatalf("a difference in %s matched", c.name)
				}
				if res.Mismatch.Category != CategoryVersionState {
					t.Errorf("category = %q, want %q", res.Mismatch.Category, CategoryVersionState)
				}
				if res.Mismatch.Section != c.section {
					t.Errorf("section = %q, want %q", res.Mismatch.Section, c.section)
				}
				for what, raw := range map[string]json.RawMessage{"expected": res.Mismatch.ExpectedJSON, "actual": res.Mismatch.ActualJSON} {
					var doc map[string]json.RawMessage
					if err := json.Unmarshal(raw, &doc); err != nil {
						t.Errorf("the %s payload is not a JSON object: %v\n%s", what, err, raw)
					}
				}
			})
		}
	})

	t.Run("the arguments are left as they were", func(t *testing.T) {
		want, got := b11aIssue(), b11aIssue()
		got.UpdatedAt = b11aWhen.Add(time.Hour)
		got.IsBlocked, got.Labels = true, []string{"l"}
		got.LeaseGrantedNode = "node-1"
		got.Dependencies[0].CreatedBy = "carol"
		want.Dependencies[1].ID = "id-1"
		beforeWant, beforeGot := *want, *got
		wantDeps := append([]*types.Dependency(nil), want.Dependencies...)
		gotDeps := append([]*types.Dependency(nil), got.Dependencies...)
		wantEdges := [2]types.Dependency{*want.Dependencies[0], *want.Dependencies[1]}
		gotEdges := [2]types.Dependency{*got.Dependencies[0], *got.Dependencies[1]}

		if _, err := CompareDurable(want, got); err != nil {
			t.Fatalf("CompareDurable: %v", err)
		}
		if !reflect.DeepEqual(*want, beforeWant) || !reflect.DeepEqual(*got, beforeGot) {
			t.Error("CompareDurable changed an issue it was given")
		}
		if !reflect.DeepEqual(want.Dependencies, wantDeps) || !reflect.DeepEqual(got.Dependencies, gotDeps) {
			t.Error("CompareDurable replaced an edge it was given")
		}
		for i := range wantEdges {
			if wantEdges[i] != *want.Dependencies[i] || gotEdges[i] != *got.Dependencies[i] {
				t.Errorf("CompareDurable changed edge %d it was given", i)
			}
		}
	})

	t.Run("the same issue is a match", func(t *testing.T) {
		i := b11aIssue()
		res, err := CompareDurable(i, i)
		if err != nil || !res.Matched {
			t.Fatalf("CompareDurable(i, i) = %+v, %v; want a match", res, err)
		}
	})
}

// b11aPoke changes a field to another value of its type, and reports whether it
// knew how. The sweep below fails when it does not, so a field of a kind this
// cannot change is found, not skipped.
func b11aPoke(field reflect.Value) bool {
	switch field.Type() {
	case reflect.TypeOf(time.Time{}):
		field.Set(reflect.ValueOf(b11aWhen.Add(time.Hour)))
	case reflect.TypeOf((*time.Time)(nil)):
		field.Set(reflect.ValueOf(b11aTime(1)))
	case reflect.TypeOf((*string)(nil)):
		field.Set(reflect.ValueOf(b11aString("changed")))
	case reflect.TypeOf((*int)(nil)):
		seven := 7
		field.Set(reflect.ValueOf(&seven))
	case reflect.TypeOf(json.RawMessage(nil)):
		field.Set(reflect.ValueOf(json.RawMessage(`{"changed":1}`)))
	case reflect.TypeOf([]string(nil)):
		field.Set(reflect.ValueOf([]string{"changed"}))
	case reflect.TypeOf([]*types.Comment(nil)):
		field.Set(reflect.ValueOf([]*types.Comment{{ID: "c-1", IssueID: "x-1", Author: "a", Text: "t", CreatedAt: b11aWhen}}))
	case reflect.TypeOf([]types.BondRef(nil)):
		field.Set(reflect.ValueOf([]types.BondRef{{SourceID: "s-1", BondType: "sequential"}}))
	case reflect.TypeOf([]*types.Dependency(nil)):
		field.Set(reflect.ValueOf([]*types.Dependency{{IssueID: "x-1", DependsOnID: "zzz-9", Type: types.DepBlocks}}))
	default:
		switch field.Kind() {
		case reflect.String:
			field.SetString(field.String() + "-changed")
		case reflect.Int, reflect.Int64:
			field.SetInt(field.Int() + 7)
		case reflect.Bool:
			field.SetBool(!field.Bool())
		default:
			return false
		}
	}
	return true
}

// TestB11aStripsExactlyTheNamedKeys sweeps every JSON key of an issue and of an
// edge. A key the comparator names as a stamp, derived or a gap can change
// without a mismatch, and every other key cannot: a field added to the types is
// compared until someone decides otherwise, and nothing the comparator ignores
// goes unnamed.
func TestB11aStripsExactlyTheNamedKeys(t *testing.T) {
	stripped := map[string]bool{}
	for _, list := range [][]string{DurableStamps(), DurableDerived(), DurableGaps()} {
		for _, key := range list {
			stripped[key] = true
		}
	}
	issueFields := b11aJSONFields(reflect.TypeOf(types.Issue{}))
	for key := range stripped {
		if _, ok := issueFields[key]; !ok {
			t.Errorf("the comparator strips %q, which an issue has no JSON key for", key)
		}
	}
	keys := make([]string, 0, len(issueFields))
	for key := range issueFields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t.Run("issue/"+key, func(t *testing.T) {
			got := b11aIssue()
			if !b11aPoke(reflect.ValueOf(got).Elem().Field(issueFields[key])) {
				t.Fatalf("this test cannot change %s", key)
			}
			res, err := CompareDurable(b11aIssue(), got)
			if err != nil {
				t.Fatalf("CompareDurable: %v", err)
			}
			switch {
			case stripped[key] && !res.Matched:
				t.Errorf("%s is named as not compared, and a change to it mismatched: %+v", key, res.Mismatch)
			case !stripped[key] && res.Matched:
				t.Errorf("%s is not named as a stamp, derived or a gap, and a change to it matched", key)
			case !stripped[key] && res.Mismatch.Category != CategoryVersionState:
				t.Errorf("category = %q, want %q", res.Mismatch.Category, CategoryVersionState)
			}
		})
	}

	depStripped := b11aSet(DurableDependencyStamps())
	depFields := b11aJSONFields(reflect.TypeOf(types.Dependency{}))
	for key := range depStripped {
		if _, ok := depFields[key]; !ok {
			t.Errorf("the comparator strips the edge key %q, which an edge has no JSON key for", key)
		}
	}
	depKeys := make([]string, 0, len(depFields))
	for key := range depFields {
		depKeys = append(depKeys, key)
	}
	sort.Strings(depKeys)
	for _, key := range depKeys {
		t.Run("dependency/"+key, func(t *testing.T) {
			got := b11aIssue()
			if !b11aPoke(reflect.ValueOf(got.Dependencies[0]).Elem().Field(depFields[key])) {
				t.Fatalf("this test cannot change %s", key)
			}
			res, err := CompareDurable(b11aIssue(), got)
			if err != nil {
				t.Fatalf("CompareDurable: %v", err)
			}
			switch {
			case depStripped[key] && !res.Matched:
				t.Errorf("edge %s is named as not compared, and a change to it mismatched: %+v", key, res.Mismatch)
			case !depStripped[key] && res.Matched:
				t.Errorf("edge %s is not named as a stamp, and a change to it matched", key)
			case !depStripped[key] && res.Mismatch.Section != SectionDependencies:
				t.Errorf("section = %q, want %q", res.Mismatch.Section, SectionDependencies)
			}
		})
	}
}

// TestB11aDurableAccessorsFollowTheStampTable keeps the three lists the version
// axis strips derived from the one table the row axis already keeps. A stamp
// column added there that is also a key of an issue is stripped here with no
// further edit.
func TestB11aDurableAccessorsFollowTheStampTable(t *testing.T) {
	issueFields := b11aJSONFields(reflect.TypeOf(types.Issue{}))
	depFields := b11aJSONFields(reflect.TypeOf(types.Dependency{}))

	// The lease overlay and the lease's granting node have no column in issues:
	// they live in a clone-local table, so the stamp table cannot name them.
	wantStamps := map[string]bool{"lease_expires_at": true, "heartbeat_at": true, "lease_granted_node": true}
	for column := range stampColumns[tableIssues] {
		if _, isKey := issueFields[column]; isKey && column != "is_blocked" {
			wantStamps[column] = true
		}
	}
	for key := range wantStamps {
		if _, ok := issueFields[key]; !ok {
			t.Errorf("%q is expected to be a stamp but is not a JSON key of an issue", key)
		}
	}
	if got := DurableStamps(); !reflect.DeepEqual(b11aSortedKeys(b11aSet(got)), b11aSortedKeys(wantStamps)) {
		t.Errorf("DurableStamps() = %v, want %v", got, b11aSortedKeys(wantStamps))
	}

	wantDeps := map[string]bool{}
	for column := range stampColumns[tableDependencies] {
		if _, ok := depFields[column]; !ok {
			t.Errorf("the dependencies stamp column %q is not a JSON key of an edge", column)
		}
		wantDeps[column] = true
	}
	if got := DurableDependencyStamps(); !reflect.DeepEqual(b11aSortedKeys(b11aSet(got)), b11aSortedKeys(wantDeps)) {
		t.Errorf("DurableDependencyStamps() = %v, want %v", got, b11aSortedKeys(wantDeps))
	}

	if got := DurableDerived(); !reflect.DeepEqual(got, []string{"is_blocked"}) {
		t.Errorf("DurableDerived() = %v, want [is_blocked]", got)
	}
	if got := DurableGaps(); !reflect.DeepEqual(got, []string{"labels"}) {
		t.Errorf("DurableGaps() = %v, want [labels]", got)
	}

	for what, list := range map[string][]string{
		"DurableStamps":           DurableStamps(),
		"DurableDependencyStamps": DurableDependencyStamps(),
		"DurableDerived":          DurableDerived(),
		"DurableGaps":             DurableGaps(),
	} {
		if !sort.StringsAreSorted(list) {
			t.Errorf("%s() = %v, want it sorted", what, list)
		}
	}
}

// TestB11aDurableSection fixes how the keys that differ name the section of the
// mismatch. The labels case cannot arise while labels are a recorded gap and
// both sides drop them, and is here so the day they are compared it is named.
func TestB11aDurableSection(t *testing.T) {
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"title"}, SectionIssue},
		{[]string{"title", "description", "status"}, SectionIssue},
		{[]string{"dependencies"}, SectionDependencies},
		{[]string{"labels"}, SectionLabels},
		{[]string{"title", "dependencies"}, "issue+dependencies"},
		{[]string{"dependencies", "title"}, "issue+dependencies"},
		{[]string{"dependencies", "labels"}, "dependencies+labels"},
		{[]string{"labels", "title"}, "issue+labels"},
		{[]string{"labels", "dependencies", "title"}, "issue+dependencies+labels"},
	} {
		if got := durableSection(c.keys); got != c.want {
			t.Errorf("durableSection(%v) = %q, want %q", c.keys, got, c.want)
		}
	}
}

func TestB11aDurableNames(t *testing.T) {
	for name, pair := range map[string][2]string{
		"CategoryVersionState": {CategoryVersionState, "version-state"},
		"SectionIssue":         {SectionIssue, "issue"},
		"SectionDependencies":  {SectionDependencies, "dependencies"},
		"SectionLabels":        {SectionLabels, "labels"},
		"SectionRecord":        {SectionRecord, "record"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q: it is stored with every mismatch, so it does not change", name, pair[0], pair[1])
		}
	}

	t.Run("a mismatch carries its section on the wire only when it has one", func(t *testing.T) {
		with, err := json.Marshal(Mismatch{Category: CategoryVersionState, Section: SectionIssue})
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if !strings.Contains(string(with), `"section":"issue"`) {
			t.Errorf("a mismatch with a section marshals as %s", with)
		}
		without, err := json.Marshal(Mismatch{Category: "unclassified"})
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if strings.Contains(string(without), "section") {
			t.Errorf("a mismatch with no section marshals as %s", without)
		}
	})
}

// TestB11aNilStates fixes what a side with no state at all means. A version
// axis that reads no record for an issue the oracle has is a mismatch of the
// record, not an error and not a pass.
func TestB11aNilStates(t *testing.T) {
	t.Run("two nothings agree", func(t *testing.T) {
		res, err := CompareDurable(nil, nil)
		if err != nil || !res.Matched {
			t.Fatalf("CompareDurable(nil, nil) = %+v, %v; want a match", res, err)
		}
	})
	for _, c := range []struct {
		name      string
		want, got *types.Issue
	}{
		{"a record the oracle has and the candidate lacks", b11aIssue(), nil},
		{"a record the candidate has and the oracle lacks", nil, b11aIssue()},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := CompareDurable(c.want, c.got)
			if err != nil {
				t.Fatalf("CompareDurable: %v", err)
			}
			if res.Matched || res.Mismatch == nil {
				t.Fatalf("one side with a state and one with none matched: %+v", res)
			}
			if res.Mismatch.Category != CategoryVersionState || res.Mismatch.Section != SectionRecord {
				t.Errorf("mismatch = category %q section %q, want %q and %q", res.Mismatch.Category, res.Mismatch.Section, CategoryVersionState, SectionRecord)
			}
			for what, raw := range map[string]json.RawMessage{"expected": res.Mismatch.ExpectedJSON, "actual": res.Mismatch.ActualJSON} {
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Errorf("the %s payload is not a JSON object: %v\n%s", what, err, raw)
				}
			}
		})
	}
}

// TestB11aNumberGate holds the version axis to the same number rule as the row
// axis: a number a float cannot hold exactly makes the pair uncomparable, which
// is neither a match nor a mismatch, and no hash is claimed for it.
func TestB11aNumberGate(t *testing.T) {
	big := json.RawMessage(`{"big": 12345678901234567890}`)
	for _, c := range []struct {
		name      string
		want, got func() *types.Issue
	}{
		{"on both sides", func() *types.Issue { i := b11aIssue(); i.Metadata = big; return i }, func() *types.Issue { i := b11aIssue(); i.Metadata = big; return i }},
		{"on one side", func() *types.Issue { i := b11aIssue(); i.Metadata = big; return i }, b11aIssue},
		{"in an edge's metadata", b11aIssue, func() *types.Issue {
			i := b11aIssue()
			i.Dependencies[0].Metadata = `{"n": 12345678901234567890}`
			return i
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := CompareDurable(c.want(), c.got())
			if err != nil {
				t.Fatalf("CompareDurable: %v", err)
			}
			if res.Matched || !res.Uncomparable() {
				t.Fatalf("a pair holding a number a float cannot hold exactly = %+v, want uncomparable", res)
			}
			if res.Mismatch.Category != CategoryNumberFidelity {
				t.Errorf("category = %q, want %q", res.Mismatch.Category, CategoryNumberFidelity)
			}
			if res.OracleHash != "" || res.CandidateHash != "" {
				t.Errorf("an uncomparable pair claims hashes %q and %q", res.OracleHash, res.CandidateHash)
			}
		})
	}
}

// TestB11aDependencyMetadataIsADocument holds an edge's metadata to what it is:
// a JSON document held in a string. The mint stores it with MySQL's spacing and
// a dolt read prints it compactly, so the text differs and the document does
// not.
func TestB11aDependencyMetadataIsADocument(t *testing.T) {
	with := func(metadata string) *types.Issue {
		i := b11aIssue()
		i.Dependencies[0].Metadata = metadata
		return i
	}
	for _, c := range []struct {
		name         string
		want, got    string
		wantMatch    bool
		wantUnusable bool
	}{
		{"spacing alone", `{"gate":"any-children"}`, `{"gate": "any-children"}`, true, false},
		{"key order alone", `{"a":1,"b":2}`, `{"b": 2, "a": 1}`, true, false},
		{"nested spacing", `{"a":{"b":[1, 2]}}`, `{"a": {"b": [1,2]}}`, true, false},
		{"a different value", `{"gate":"any-children"}`, `{"gate": "all-children"}`, false, false},
		{"a different key", `{"gate":"any-children"}`, `{"kind": "any-children"}`, false, false},
		{"text that is not JSON, equal", `not json`, `not json`, true, false},
		{"text that is not JSON, different", `not json`, `not json at all`, false, false},
		{"the empty document and nothing", `{}`, ``, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := CompareDurable(with(c.want), with(c.got))
			if err != nil {
				t.Fatalf("CompareDurable: %v", err)
			}
			if res.Matched != c.wantMatch {
				t.Fatalf("matched = %v, want %v (%+v)", res.Matched, c.wantMatch, res.Mismatch)
			}
			if !c.wantMatch && res.Mismatch.Section != SectionDependencies {
				t.Errorf("section = %q, want %q", res.Mismatch.Section, SectionDependencies)
			}
		})
	}
}

// TestB11aCompareStaysBelowVersions keeps the layering the version axis relies
// on: compare knows the types it compares and the number rule, and nothing that
// reads a store or decides what the translator replays. The projection and the
// reader are in the versions package, which imports compare, so an import the
// other way is a cycle waiting to happen.
func TestB11aCompareStaysBelowVersions(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		parsed, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, banned := range []string{
				"github.com/steveyegge/beads/internal/replay/versions",
				"github.com/steveyegge/beads/internal/replay/translate",
				"github.com/steveyegge/beads/internal/replay/driver",
			} {
				if path == banned {
					t.Errorf("%s imports %s; compare sits below it", name, path)
				}
			}
		}
	}
}
