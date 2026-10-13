package compare

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// This file compares two issues as the version history keeps them: the document
// the mint stores in a version's durable_state, which is a types.Issue. The row
// comparison above reads a table's columns; this one reads the issue those
// columns stand for, on both sides, so that the stored document and the rows that
// were replayed can be held to each other.

// CategoryVersionState is the Mismatch.Category of a pair of issues whose stored
// version and whose replayed rows disagree. The string is stored with every
// mismatch, so it does not change.
const CategoryVersionState = "version-state"

// The sections of an issue a version-state mismatch names. A section is a
// top-level key of the stored document that holds a part of the issue of its own;
// every other key is the issue itself. SectionRecord is a difference in whether
// there is a stored document at all.
const (
	SectionIssue        = "issue"
	SectionDependencies = "dependencies"
	SectionLabels       = "labels"
	SectionRecord       = "record"
)

// The keys of a stored document that no column of the issues table supplies, and
// the one whose column the row comparison leaves out for a reason of its own.
const (
	// derivedIssueKey is the one stamp column that is not a stamp here: the store
	// derives it from the edge set, so a stored document may or may not carry it.
	derivedIssueKey = "is_blocked"
	// gapKey is the labels section, which the replay does not cover yet.
	gapKey = "labels"
	// dependenciesKey is the section that holds the issue's own edges.
	dependenciesKey = "dependencies"
)

// leaseKeys are the stamps of the lease overlay. Their values are written into the
// stored document, but the columns they come from live in a clone-local table, so
// the stamp table, which names the columns of issues, cannot name them.
var leaseKeys = []string{"lease_expires_at", "heartbeat_at", "lease_granted_node"}

var (
	issueKeys      = jsonKeys(reflect.TypeOf(types.Issue{}))
	dependencyKeys = jsonKeys(reflect.TypeOf(types.Dependency{}))

	durableStamps           = deriveDurableStamps()
	durableDependencyStamps = sortedKeys(stampColumns[tableDependencies])
)

// jsonKeys maps each JSON key of a struct type to the index of its field.
func jsonKeys(typ reflect.Type) map[string]int {
	keys := make(map[string]int, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys[name] = i
	}
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deriveDurableStamps is the stamp table of the issues columns that are also keys
// of an issue, with the lease overlay's keys.
func deriveDurableStamps() []string {
	set := map[string]bool{}
	for column := range stampColumns[tableIssues] {
		if _, isKey := issueKeys[column]; isKey && column != derivedIssueKey {
			set[column] = true
		}
	}
	for _, key := range leaseKeys {
		set[key] = true
	}
	return sortedKeys(set)
}

// DurableStamps are the keys of an issue that the version axis never compares: the
// values the store writes about a write, not about the issue, which differ between
// the stored document and the replayed rows by construction. They are the stamp
// columns the row comparison already leaves out, so a column added to that table
// that is also a key of an issue is left out here with no further edit, and the
// keys of the lease overlay, which have no column. Sorted.
func DurableStamps() []string { return append([]string{}, durableStamps...) }

// DurableDependencyStamps are the keys of an edge that the version axis never
// compares: the stamp columns of the dependencies table. Sorted.
func DurableDependencyStamps() []string { return append([]string{}, durableDependencyStamps...) }

// DurableDerived are the keys of an issue that the store derives from other state
// and may leave out of a stored document: is_blocked, which follows the edge set
// the document already holds. Sorted.
func DurableDerived() []string { return []string{derivedIssueKey} }

// DurableGaps are the keys of an issue whose table the replay does not cover, so
// the replayed rows cannot supply them: labels, until the translator replays them.
// Sorted.
func DurableGaps() []string { return []string{gapKey} }

// strippedIssueKeys are the keys of an issue the comparison zeroes on both sides.
var strippedIssueKeys = func() []string {
	set := map[string]bool{}
	for _, list := range [][]string{durableStamps, DurableDerived(), DurableGaps()} {
		for _, key := range list {
			set[key] = true
		}
	}
	return sortedKeys(set)
}()

// CompareDurable compares two issues as stored versions: the stamps, the derived
// state and the gaps are zeroed on both sides, the rest is marshaled and handed to
// Compare. want is the issue the oracle's rows stand for and got the stored one.
//
// An edge's metadata is a JSON document held in a string, which the mint stores
// with MySQL's spacing and a read prints compactly, so a document that parses is
// compared as the document and text that does not parse is compared as text. A
// number outside what can be compared exactly, at the top level or in an edge's
// metadata, makes the pair uncomparable, as it does for the rows.
//
// A mismatch is a version-state mismatch. Its Section names where the two issues
// differ: the issue's own fields, its dependencies or its labels, joined with "+"
// when more than one differs, or the record when only one side has an issue.
// Neither argument is changed.
func CompareDurable(want, got *types.Issue) (Result, error) {
	w, err := render(want)
	if err != nil {
		return Result{}, fmt.Errorf("the expected issue: %w", err)
	}
	g, err := render(got)
	if err != nil {
		return Result{}, fmt.Errorf("the actual issue: %w", err)
	}

	if (want == nil) != (got == nil) {
		// Whether there is a stored document at all is a finding before any number in
		// it is, and there is nothing on one side to compare the other with.
		return Result{Mismatch: &Mismatch{
			Category:     CategoryVersionState,
			Section:      SectionRecord,
			ExpectedJSON: w.doc,
			ActualJSON:   g.doc,
		}}, nil
	}
	if w.refusesNumber() || g.refusesNumber() {
		return Result{Mismatch: &Mismatch{
			Category:     CategoryNumberFidelity,
			ExpectedJSON: w.doc,
			ActualJSON:   g.doc,
		}}, nil
	}

	wantCompared, err := w.compared()
	if err != nil {
		return Result{}, fmt.Errorf("the expected issue: %w", err)
	}
	gotCompared, err := g.compared()
	if err != nil {
		return Result{}, fmt.Errorf("the actual issue: %w", err)
	}
	res, err := Compare(wantCompared, gotCompared)
	if err != nil {
		return Result{}, err
	}
	if res.Matched {
		return res, nil
	}
	// The documents are shown as they were rendered, not as they were compared.
	res.Mismatch.ExpectedJSON, res.Mismatch.ActualJSON = w.doc, g.doc
	if res.Uncomparable() {
		return res, nil
	}

	keys, err := differingKeys(wantCompared, gotCompared)
	if err != nil {
		return Result{}, err
	}
	res.Mismatch.Category = CategoryVersionState
	res.Mismatch.Section = durableSection(keys)
	return res, nil
}

// side is one issue of a comparison with the keys the comparison leaves out zeroed:
// a copy that shares nothing with the issue it was made from that the comparison
// changes, so the caller's issue, and the edges it may share, are left as they
// were. A nil issue is no copy, and renders as the empty document.
type side struct {
	issue *types.Issue
	doc   []byte
}

func render(issue *types.Issue) (side, error) {
	if issue == nil {
		return side{doc: []byte("{}")}, nil
	}
	out := *issue
	v := reflect.ValueOf(&out).Elem()
	for _, key := range strippedIssueKeys {
		index, ok := issueKeys[key]
		if !ok {
			return side{}, fmt.Errorf("the comparison leaves out %q, which an issue has no JSON key for", key)
		}
		field := v.Field(index)
		field.Set(reflect.Zero(field.Type()))
	}
	if issue.Dependencies != nil {
		out.Dependencies = make([]*types.Dependency, len(issue.Dependencies))
		for i, edge := range issue.Dependencies {
			if edge == nil {
				continue
			}
			copied := *edge
			ev := reflect.ValueOf(&copied).Elem()
			for _, key := range durableDependencyStamps {
				index, ok := dependencyKeys[key]
				if !ok {
					return side{}, fmt.Errorf("the comparison leaves out the edge key %q, which an edge has no JSON key for", key)
				}
				field := ev.Field(index)
				field.Set(reflect.Zero(field.Type()))
			}
			out.Dependencies[i] = &copied
		}
	}
	doc, err := json.Marshal(&out)
	if err != nil {
		return side{}, fmt.Errorf("marshal: %w", err)
	}
	return side{issue: &out, doc: doc}, nil
}

// refusesNumber reports whether any edge's metadata holds a number outside what
// can be compared exactly. Text that is not a document holds none.
func (s side) refusesNumber() bool {
	if s.issue == nil {
		return false
	}
	for _, edge := range s.issue.Dependencies {
		if edge == nil || !json.Valid([]byte(edge.Metadata)) {
			continue
		}
		if issueops.ReplayRefuseUnrepresentableIntegers([]byte(edge.Metadata)) != nil {
			return true
		}
	}
	return false
}

// compared is the document Compare is given: the rendered issue, with the metadata
// of each edge that parses as a document canonicalized, because spacing and key
// order are how a document was written and not what it says.
func (s side) compared() ([]byte, error) {
	if s.issue == nil || len(s.issue.Dependencies) == 0 {
		return s.doc, nil
	}
	out := *s.issue
	out.Dependencies = make([]*types.Dependency, len(s.issue.Dependencies))
	for i, edge := range s.issue.Dependencies {
		if edge == nil {
			continue
		}
		copied := *edge
		if json.Valid([]byte(copied.Metadata)) {
			// A document that cannot be canonicalized, such as one with a repeated key, is
			// compared as the text it was written as.
			if canonical, err := Canonicalize([]byte(copied.Metadata)); err == nil {
				copied.Metadata = string(canonical)
			}
		}
		out.Dependencies[i] = &copied
	}
	doc, err := json.Marshal(&out)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return doc, nil
}

// differingKeys returns the top-level keys at which two documents differ, those
// present on one side only included, sorted.
func differingKeys(want, got []byte) ([]string, error) {
	var wantKeys, gotKeys map[string]json.RawMessage
	if err := json.Unmarshal(want, &wantKeys); err != nil {
		return nil, fmt.Errorf("decoding the expected document: %w", err)
	}
	if err := json.Unmarshal(got, &gotKeys); err != nil {
		return nil, fmt.Errorf("decoding the actual document: %w", err)
	}
	differ := map[string]bool{}
	for key, wantValue := range wantKeys {
		gotValue, ok := gotKeys[key]
		if !ok {
			differ[key] = true
			continue
		}
		wantCanonical, err := Canonicalize(wantValue)
		if err != nil {
			return nil, fmt.Errorf("canonicalizing %s of the expected document: %w", key, err)
		}
		gotCanonical, err := Canonicalize(gotValue)
		if err != nil {
			return nil, fmt.Errorf("canonicalizing %s of the actual document: %w", key, err)
		}
		if string(wantCanonical) != string(gotCanonical) {
			differ[key] = true
		}
	}
	for key := range gotKeys {
		if _, ok := wantKeys[key]; !ok {
			differ[key] = true
		}
	}
	return sortedKeys(differ), nil
}

// durableSection names where documents that differ at keys differ: the issue's own
// fields, its dependencies, its labels, in that order and joined with "+". Every
// key that is not a section of its own is the issue.
func durableSection(keys []string) string {
	var issue, dependencies, labels bool
	for _, key := range keys {
		switch key {
		case dependenciesKey:
			dependencies = true
		case gapKey:
			labels = true
		default:
			issue = true
		}
	}
	var sections []string
	if issue {
		sections = append(sections, SectionIssue)
	}
	if dependencies {
		sections = append(sections, SectionDependencies)
	}
	if labels {
		sections = append(sections, SectionLabels)
	}
	if len(sections) == 0 {
		return SectionIssue
	}
	return strings.Join(sections, "+")
}
