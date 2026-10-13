package versions

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/replay/translate"
	"github.com/steveyegge/beads/internal/types"
)

// b11aJSONKey is one key of a struct's JSON form, as its field tag declares it.
type b11aJSONKey struct {
	name      string
	omitEmpty bool
	kind      reflect.Kind
}

// b11aTypeKeys lists the keys encoding/json writes for a struct type. A field
// with no tag is a failure: it would be written under its Go name, and nothing
// here would classify it.
func b11aTypeKeys(t *testing.T, typ reflect.Type) []b11aJSONKey {
	t.Helper()
	var keys []b11aJSONKey
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			t.Fatalf("%s.%s has no json tag", typ.Name(), f.Name)
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		keys = append(keys, b11aJSONKey{
			name:      name,
			omitEmpty: strings.Contains(","+opts+",", ",omitempty,"),
			kind:      f.Type.Kind(),
		})
	}
	return keys
}

func b11aKeyNamed(t *testing.T, keys []b11aJSONKey, name string) b11aJSONKey {
	t.Helper()
	for _, k := range keys {
		if k.name == name {
			return k
		}
	}
	t.Fatalf("the type has no JSON key %q", name)
	return b11aJSONKey{}
}

func b11aKeysOfClass(fields []Field, class Class) []string {
	var keys []string
	for _, f := range fields {
		if f.Class == class {
			keys = append(keys, f.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

func b11aSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func b11aContains(list []string, want string) bool {
	for _, have := range list {
		if have == want {
			return true
		}
	}
	return false
}

// b11aCheckTable holds one drift table to the type it classifies: every JSON key
// of the type is in exactly one entry, and the table names no key the type does
// not have.
func b11aCheckTable(t *testing.T, typ reflect.Type, fields []Field, allowed []Class) {
	t.Helper()
	count := map[string]int{}
	for _, f := range fields {
		count[f.Key]++
		if !b11aContainsClass(allowed, f.Class) {
			t.Errorf("%s: the entry for %q has class %q, want one of %v", typ.Name(), f.Key, f.Class, allowed)
		}
	}
	typeKeys := map[string]bool{}
	for _, k := range b11aTypeKeys(t, typ) {
		typeKeys[k.name] = true
		switch count[k.name] {
		case 1:
		case 0:
			t.Errorf("%s: the JSON key %q is in no entry of the drift table; classify it", typ.Name(), k.name)
		default:
			t.Errorf("%s: the JSON key %q is in %d entries of the drift table, want exactly one", typ.Name(), k.name, count[k.name])
		}
	}
	for key := range count {
		if !typeKeys[key] {
			t.Errorf("%s: the drift table names %q, which the type has no JSON key for", typ.Name(), key)
		}
	}
}

func b11aContainsClass(list []Class, want Class) bool {
	for _, have := range list {
		if have == want {
			return true
		}
	}
	return false
}

// TestB11aProjectionDrift is the guard that keeps the projection honest as the
// types it mirrors change. Every JSON key of an issue and of a dependency is
// classified exactly once: compared, a stamp (differs by construction, zeroed on
// both sides), derived, absent from durable_state, or a recorded gap. A key
// added to the types without an entry fails here, and so does an entry for a key
// that has gone, a column the store no longer has, and a column nobody accounts
// for.
func TestB11aProjectionDrift(t *testing.T) {
	issueType := reflect.TypeOf(types.Issue{})
	depType := reflect.TypeOf(types.Dependency{})
	allClasses := []Class{ClassCompared, ClassStamp, ClassDerived, ClassNotInDurableState, ClassGap}

	t.Run("EveryIssueKeyIsClassifiedOnce", func(t *testing.T) {
		b11aCheckTable(t, issueType, IssueFields(), allClasses)
	})

	t.Run("EveryDependencyKeyIsClassifiedOnce", func(t *testing.T) {
		b11aCheckTable(t, depType, DependencyFields(), []Class{ClassCompared, ClassStamp})
	})

	t.Run("EntriesSayWhyAndWhere", func(t *testing.T) {
		for _, table := range []struct {
			name   string
			fields []Field
		}{{"issue", IssueFields()}, {"dependency", DependencyFields()}} {
			for _, f := range table.fields {
				switch {
				case f.Class != ClassCompared && strings.TrimSpace(f.Reason) == "":
					t.Errorf("%s %q is %s and carries no reason", table.name, f.Key, f.Class)
				case f.Class == ClassCompared && len(f.Columns) == 0 && strings.TrimSpace(f.Reason) == "":
					t.Errorf("%s %q is compared, names no column, and carries no reason", table.name, f.Key)
				case f.Class == ClassCompared && len(f.Columns) == 0 && f.Key != "dependencies":
					t.Errorf("%s %q is compared but no column of the store backs it", table.name, f.Key)
				}
			}
		}
		for _, f := range IssueFields() {
			if f.Key == "dependencies" && (f.Class != ClassCompared || len(f.Columns) != 0) {
				t.Errorf("dependencies is a section of its own, compared and backed by a table rather than a column; got class %q columns %v", f.Class, f.Columns)
			}
		}
	})

	t.Run("ClassesAgreeWithCompare", func(t *testing.T) {
		// compare strips what the table calls a stamp or derived before it compares,
		// and compare is a different package: the two lists are the same list.
		for _, c := range []struct {
			what   string
			fields []Field
			class  Class
			want   []string
		}{
			{"issue stamps", IssueFields(), ClassStamp, compare.DurableStamps()},
			{"dependency stamps", DependencyFields(), ClassStamp, compare.DurableDependencyStamps()},
			{"derived keys", IssueFields(), ClassDerived, compare.DurableDerived()},
			{"gaps", IssueFields(), ClassGap, compare.DurableGaps()},
		} {
			got := b11aKeysOfClass(c.fields, c.class)
			want := b11aSorted(c.want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: the drift table says %v, compare strips %v", c.what, got, want)
			}
		}
	})

	t.Run("SectionsFollowTheTablePolicy", func(t *testing.T) {
		for _, table := range []string{"issues", "dependencies"} {
			policy, ok := translate.PolicyFor(table)
			if !ok || policy.Class != translate.TableReplayed {
				t.Errorf("the projection compares the %s section, but the translator's policy for that table is %+v (known %v)", table, policy, ok)
			}
		}
		for _, f := range IssueFields() {
			if f.Class != ClassGap {
				continue
			}
			policy, ok := translate.PolicyFor(f.Key)
			if !ok {
				t.Errorf("%q is recorded as a gap, but it is not a table the translator has a policy for", f.Key)
				continue
			}
			if policy.Class == translate.TableReplayed {
				t.Errorf("%q is recorded as a gap, but the translator replays that table now: the projection has to compare it", f.Key)
			}
		}
	})

	t.Run("ColumnsMatchAFreshStore", func(t *testing.T) {
		replaytest.Require(t, replaytest.NeedDolt|replaytest.NeedBd)
		replaytest.Isolate(t)
		bin := replaytest.BdBin(t)
		project := replaytest.InitBdProject(t, "b11a-drift", bin)
		data := replaytest.DataDir(t, project)
		head := replaytest.HeadCommit(t, data)
		ctx := context.Background()

		issueColumns, err := oracle.Columns(ctx, data, head, "issues")
		if err != nil || len(issueColumns) == 0 {
			t.Fatalf("reading the columns of issues: %v (%d columns)", err, len(issueColumns))
		}
		depColumns, err := oracle.Columns(ctx, data, head, "dependencies")
		if err != nil || len(depColumns) == 0 {
			t.Fatalf("reading the columns of dependencies: %v (%d columns)", err, len(depColumns))
		}

		named := map[string]string{}
		for _, f := range IssueFields() {
			for _, c := range f.Columns {
				if !b11aContains(issueColumns, c) {
					t.Errorf("the entry for %q names the column %q, which the issues table does not have", f.Key, c)
				}
				if other, dup := named[c]; dup {
					t.Errorf("the issues column %q is named by both %q and %q", c, other, f.Key)
				}
				named[c] = f.Key
			}
		}
		uncarried := UncarriedColumns()
		for _, c := range issueColumns {
			_, isNamed := named[c]
			reason, isUncarried := uncarried[c]
			switch {
			case isNamed && isUncarried:
				t.Errorf("the issues column %q is both carried (by %q) and listed as uncarried", c, named[c])
			case !isNamed && !isUncarried:
				t.Errorf("the issues column %q is neither carried by a key nor listed as uncarried; account for it", c)
			case isUncarried && strings.TrimSpace(reason) == "":
				t.Errorf("the issues column %q is listed as uncarried and carries no reason", c)
			}
		}
		for c := range uncarried {
			if !b11aContains(issueColumns, c) {
				t.Errorf("%q is listed as an uncarried issues column, but the table has no such column", c)
			}
		}

		depNamed := map[string]string{}
		for _, f := range DependencyFields() {
			for _, c := range f.Columns {
				if !b11aContains(depColumns, c) {
					t.Errorf("the entry for dependency %q names the column %q, which the dependencies table does not have", f.Key, c)
				}
				depNamed[c] = f.Key
			}
		}
		for _, c := range depColumns {
			if _, ok := depNamed[c]; !ok {
				t.Errorf("the dependencies column %q is named by no dependency key; every one is carried", c)
			}
		}
	})

	t.Run("ExcludesAreRecorded", func(t *testing.T) {
		names := map[string]bool{}
		for _, e := range Excludes() {
			if strings.TrimSpace(e.Name) == "" {
				t.Errorf("an exclude has no name: %+v", e)
			}
			if names[e.Name] {
				t.Errorf("the exclude %q is listed twice", e.Name)
			}
			names[e.Name] = true
			if strings.TrimSpace(e.Reason) == "" {
				t.Errorf("the exclude %q carries no reason", e.Name)
			}
		}
		for _, want := range []string{"comments", "product-read-surface", "is_blocked", "wisps", "labels"} {
			if !names[want] {
				t.Errorf("the harness does not record %q as outside what the version axis checks", want)
			}
		}
	})

	t.Run("LossyBasisIsTheDocumentedOne", func(t *testing.T) {
		const want = "domain-model equivalence (ScanIssueFrom maps NULL to zero)"
		if LossyBasis != want {
			t.Errorf("LossyBasis = %q, want %q", LossyBasis, want)
		}
	})
}
