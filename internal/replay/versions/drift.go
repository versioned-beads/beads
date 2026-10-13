package versions

import (
	"reflect"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// This file is the drift table: a decision about every key of the document a
// version stores, and about every column of the issues table that has no key.
// Tests hold the table to the types it classifies and to the schema of a fresh
// store, so a key or a column added later fails a build until someone decides
// what the version axis does with it. The projection and the comparator are two
// implementations of the table; the table is what says they agree.

// Class says what the version axis does with a key of a stored document.
type Class string

const (
	// ClassCompared keys are supplied by the rows and compared with the stored
	// document.
	ClassCompared Class = "compared"
	// ClassStamp keys are values the store writes about a write rather than about
	// the issue: they differ between a stored version and replayed rows by
	// construction, so both sides are zeroed before comparing.
	ClassStamp Class = "stamp"
	// ClassDerived keys are values the store derives from other state and may or may
	// not have written, so both sides are zeroed before comparing.
	ClassDerived Class = "derived"
	// ClassNotInDurableState keys are fields of the type that the mint never fills,
	// so a stored document never carries them.
	ClassNotInDurableState Class = "not-in-durable-state"
	// ClassGap keys are parts of the stored document whose table the replay does not
	// cover yet, so the rows cannot supply them: a recorded gap, not a pass.
	ClassGap Class = "gap"
)

// Field is the decision about one JSON key of a type.
type Field struct {
	// Key is the JSON key.
	Key string
	// Class says what the axis does with the key.
	Class Class
	// Columns are the columns of the key's table that supply it. A key with none is
	// supplied by no column: a section of its own, a gap, or a value the rows do not
	// hold.
	Columns []string
	// Reason says why the key is not compared, or, for the one compared key that no
	// column backs, what backs it. A compared key with a column needs none.
	Reason string
}

func compared(key string, columns ...string) Field {
	return Field{Key: key, Class: ClassCompared, Columns: columns}
}

func stamp(key, reason string, columns ...string) Field {
	return Field{Key: key, Class: ClassStamp, Columns: columns, Reason: reason}
}

func notInDurableState(key, reason string) Field {
	return Field{Key: key, Class: ClassNotInDurableState, Reason: reason}
}

// issueFields classifies every JSON key of types.Issue, in the order the type
// declares them.
var issueFields = []Field{
	compared("id", "id"),
	compared("title", "title"),
	compared("description", "description"),
	compared("design", "design"),
	compared("acceptance_criteria", "acceptance_criteria"),
	compared("notes", "notes"),
	compared("spec_id", "spec_id"),
	compared("status", "status"),
	compared("priority", "priority"),
	compared("issue_type", "issue_type"),
	{
		Key: "is_blocked", Class: ClassDerived, Columns: []string{"is_blocked"},
		Reason: "derived from the edge set; the mint stores it only while it is true, and the column goes stale until the row is next written, so a stored version cannot be held to it",
	},
	compared("assignee", "assignee"),
	compared("owner", "owner"),
	compared("estimated_minutes", "estimated_minutes"),
	stamp("created_at", "wall clock at insert", "created_at"),
	compared("created_by", "created_by"),
	stamp("updated_at", "wall clock at every write", "updated_at"),
	stamp("started_at", "wall clock when work started", "started_at"),
	stamp("closed_at", "wall clock at close", "closed_at"),
	compared("close_reason", "close_reason"),
	stamp("closed_by_session", "the session that wrote the close, not a property of the issue", "closed_by_session"),
	stamp("lease_expires_at", "the claim lease overlay, written into the document from a clone-local table that no column of issues holds"),
	stamp("heartbeat_at", "the claim lease overlay, written into the document from a clone-local table that no column of issues holds"),
	stamp("lease_granted_node", "the claim lease overlay, written into the document from a clone-local table that no column of issues holds"),
	compared("due_at", "due_at"),
	compared("defer_until", "defer_until"),
	compared("external_ref", "external_ref"),
	compared("source_system", "source_system"),
	compared("metadata", "metadata"),
	compared("compaction_level", "compaction_level"),
	stamp("compacted_at", "wall clock at compaction", "compacted_at"),
	stamp("compacted_at_commit", "a commit hash from the compacting clone's own history, which differs between clones by construction", "compacted_at_commit"),
	compared("original_size", "original_size"),
	{
		Key: "labels", Class: ClassGap,
		Reason: "the labels table is not replayed until the translator covers it; the stored document carries the labels and the rows of an issue's view do not, so the section is recorded as a gap rather than compared",
	},
	{
		Key: "dependencies", Class: ClassCompared,
		Reason: "the edges the issue owns, supplied by the rows of the dependencies table whose issue_id is this issue and compared key by key as DependencyFields says",
	},
	notInDurableState("comments", "comments are a table of their own, and the loader the mint snapshots through does not hydrate them, so a stored document never carries one"),
	compared("sender", "sender"),
	compared("ephemeral", "ephemeral"),
	compared("no_history", "no_history"),
	compared("wisp_type", "wisp_type"),
	compared("storage_class", "storage_class"),
	compared("pinned", "pinned"),
	compared("is_template", "is_template"),
	notInDurableState("bonded_from", "no column of issues holds it and the loader never fills it, so a stored document never carries it"),
	compared("await_type", "await_type"),
	compared("await_id", "await_id"),
	compared("timeout", "timeout_ns"),
	compared("waiters", "waiters"),
	notInDurableState("source_formula", "no column of issues holds it and the loader never fills it, so a stored document never carries it"),
	notInDurableState("source_location", "no column of issues holds it and the loader never fills it, so a stored document never carries it"),
	compared("mol_type", "mol_type"),
	compared("work_type", "work_type"),
	compared("event_kind", "event_kind"),
	compared("actor", "actor"),
	compared("target", "target"),
	compared("payload", "payload"),
}

// dependencyFields classifies every JSON key of types.Dependency, in the order the
// type declares them. Every column of the dependencies table is named by one.
var dependencyFields = []Field{
	stamp("id", "a surrogate key generated when the edge is written; the mint does not select it, so durable_state never carries it", "id"),
	compared("issue_id", "issue_id"),
	compared("depends_on_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"),
	compared("type", "type"),
	stamp("created_at", "wall clock at insert", "created_at"),
	stamp("created_by", "the identity that wrote the edge, not a property of the edge", "created_by"),
	compared("metadata", "metadata"),
	compared("thread_id", "thread_id"),
}

// uncarriedColumns are the columns of the issues table that no key of a stored
// document is supplied by. types.Issue has no JSON key for them, so the mint never
// stores them and the version axis has nothing to compare; the row axis still
// does.
var uncarriedColumns = map[string]string{
	"content_hash":             "types.Issue carries it as a field the document never serializes; it is derived from the other columns on every write",
	"source_repo":              "types.Issue carries it as a field the document never serializes",
	"hook_bead":                "types.Issue has no field for it, so no stored document carries it",
	"role_bead":                "types.Issue has no field for it, so no stored document carries it",
	"agent_state":              "types.Issue has no field for it, so no stored document carries it",
	"last_activity":            "types.Issue has no field for it, so no stored document carries it",
	"role_type":                "types.Issue has no field for it, so no stored document carries it",
	"rig":                      "types.Issue has no field for it, so no stored document carries it",
	"row_lock":                 "types.Issue carries it as the row version, a field the document never serializes",
	"current_revision":         "the local ordinal the mint advances after it records a version; types.Issue has no field for it",
	"participation_generation": "the store's own epoch stamp, the write fence's marker; types.Issue has no field for it",
}

// IssueFields returns the decision about every JSON key of an issue.
func IssueFields() []Field { return copyFields(issueFields) }

// DependencyFields returns the decision about every JSON key of a dependency.
func DependencyFields() []Field { return copyFields(dependencyFields) }

// UncarriedColumns returns the columns of the issues table that no key of a stored
// document is supplied by, each with the reason it has none.
func UncarriedColumns() map[string]string {
	out := make(map[string]string, len(uncarriedColumns))
	for column, reason := range uncarriedColumns {
		out[column] = reason
	}
	return out
}

func copyFields(fields []Field) []Field {
	out := make([]Field, len(fields))
	for i, f := range fields {
		out[i] = f
		out[i].Columns = append([]string(nil), f.Columns...)
	}
	return out
}

// LossyBasis says why a field on the lossy list is not a finding: the store's own
// reader maps NULL to the zero value, so a version the mint stored from a row that
// holds NULL and one it stored from a row that holds the zero are the same
// document.
const LossyBasis = "domain-model equivalence (ScanIssueFrom maps NULL to zero)"

// LossyField is a key whose zero value cannot be told from NULL once the issue has
// been through the mint, and so cannot be told apart on the version axis.
type LossyField struct {
	Key string `json:"key"`
	// Tag is the JSON tag option that loses the difference: omitempty leaves a
	// zero value out of the document, so the document says the same for NULL, for
	// the empty string, for 0 and for false.
	Tag string `json:"tag"`
}

// lossy is every compared key backed by a column whose Go field is a value type
// that the JSON tag omits when it is empty. A pointer keeps the difference, and so
// does a field without omitempty.
var lossy = deriveLossy()

func deriveLossy() []LossyField {
	out := []LossyField{}
	collect := func(prefix string, typ reflect.Type, fields []Field) {
		for _, f := range fields {
			if f.Class != ClassCompared || len(f.Columns) == 0 {
				continue
			}
			if kind, omitEmpty, ok := jsonField(typ, f.Key); ok && omitEmpty && kind != reflect.Ptr {
				out = append(out, LossyField{Key: prefix + f.Key, Tag: "omitempty"})
			}
		}
	}
	collect("", reflect.TypeOf(types.Issue{}), issueFields)
	collect("dependencies.", reflect.TypeOf(types.Dependency{}), dependencyFields)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// jsonField finds the field of a struct type that a JSON key names, and says what
// its kind is and whether its tag leaves it out when it is empty.
func jsonField(typ reflect.Type, key string) (kind reflect.Kind, omitEmpty, ok bool) {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != key {
			continue
		}
		return field.Type.Kind(), strings.Contains(","+options+",", ",omitempty,"), true
	}
	return reflect.Invalid, false, false
}

// Lossy returns the keys of an issue, and of its dependencies as "dependencies."
// and the key, whose NULL the mint's document cannot tell from the zero value. The
// projection reads such a NULL as the zero, so a replay that wrote NULL where a
// stored version has the zero, or the reverse, agrees on this axis; the row axis,
// which reads the cells themselves, is where the difference shows. A harness run
// reports the list once, with LossyBasis, and never per row. Sorted.
func Lossy() []LossyField {
	return append([]LossyField{}, lossy...)
}

// Exclude is something the version axis does not check, and why.
type Exclude struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

var excludes = []Exclude{
	{"comments", "comments are a table of their own that a stored document never carries, so the axis has nothing to hold them to"},
	{"product-read-surface", "bd versions and the as-of read by time are the product's own commands over the same history; the axis reads issue_versions directly and does not run them"},
	{"is_blocked", "derived from the edge set and stored only while true, so a stored version goes stale where the row does not; both sides are zeroed rather than compared"},
	{"wisps", "wisps, and issues marked no-history, are never versioned, so there is no stored version to hold them to; the axis reports them as not run"},
	{"labels", "a recorded gap: the translator does not replay the labels table yet, so the rows of a view cannot supply the labels a stored version carries"},
	{"uncarried-columns", "the columns of the issues table that types.Issue has no key for never reach a stored version; the row axis still compares them (see UncarriedColumns)"},
}

// Excludes returns what the version axis does not check, once per run and not per
// row: each is a standing fact about the store or the harness.
func Excludes() []Exclude {
	return append([]Exclude{}, excludes...)
}
