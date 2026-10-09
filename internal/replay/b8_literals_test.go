package replay_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
)

// B8.NoOverrideLiterals: the harness cannot switch off a refusal of the product
// that its seeding depends on, and cannot send a store anywhere, by a stray line.
// Three kinds of text are guarded:
//
//   - the names of the variables that switch a refusal off;
//   - the dolt and bd verbs that send a store's state out or take someone else's
//     in, as the shared deny-list in doltcli lists them;
//   - a force flag on a migrate verb.
//
// Each may be spelled only in the file that owns it (the shared deny-list) and in
// the tests that assert it is stripped or refused. An entry that no longer holds
// any is an error too, so the list of places cannot outlive what it excuses.

const (
	famName  = "override name"
	famVerb  = "outbound verb"
	famForce = "force flag on a migrate verb"
)

// overrideNames are assembled from fragments so this file does not match its own
// scan, as hygienePatterns is.
var overrideNames = []string{
	"BD_ALLOW_" + "REMOTE_MIGRATE",
	"BEADS_SKIP_" + "IDENTITY_CHECK",
	"BD_IGNORE_" + "SCHEMA_SKEW",
}

const (
	migrateWord = "migra" + "te"
	forceFlag   = "--for" + "ce"
)

// literalAllowed is, for each kind of text, the files that may hold it and why.
var literalAllowed = map[string]map[string]string{
	famName: {
		"internal/replay/doltcli/doltcli.go":                     "the shared deny-list, the one place outside a test that spells them",
		"internal/replay/doltcli/b8_test.go":                     "asserts the names are stripped from the environment of every child",
		"internal/replay/driver/b8_controls_test.go":             "asserts a seed still refuses with the names set in the ambient environment",
		"internal/replay/scenarios/compatwindow/fixture_test.go": "a scenario that sets the skew hatch on a store it built itself and never seeds",
	},
	famVerb: {
		"internal/replay/doltcli/doltcli.go":         "the shared deny-list itself",
		"internal/replay/doltcli/b8_test.go":         "asserts the runner refuses every verb on the list",
		"internal/replay/driver/b8_controls_test.go": "plants a remote the seeding must refuse, and checks it does",
		"internal/replay/driver/b8_run_test.go":      "plants a remote and a backup mid-run, in a shell stand-in, to show the end of the run refuses them",
		"internal/replay/driver/b8_fixture_test.go":  "builds the oracle fixture with the remote and the backup the seeding takes away",
		"internal/replay/driver/e2e_test.go":         "moves history between two local sandbox repositories to build a fixture",
	},
	famForce: {},
}

type literalHit struct {
	family string
	line   int
	text   string
}

// litItem is one argument of a call, or one element of a composite literal: its
// text when it is a string literal, and nothing but its place when it is not.
type litItem struct {
	text string
	lit  bool
}

type litGroup struct {
	pos    token.Pos
	callee string // the called function's own name, empty for a composite literal
	items  []litItem
}

func stringLit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

func itemsOf(exprs []ast.Expr) []litItem {
	items := make([]litItem, len(exprs))
	for i, e := range exprs {
		items[i].text, items[i].lit = stringLit(e)
	}
	return items
}

func calleeName(fun ast.Expr) string {
	switch fn := fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// hasRun reports whether the words are among the items, each in the place after
// the one before, with no argument that is not a literal between them.
func hasRun(items []litItem, words []string) bool {
	for i := 0; i+len(words) <= len(items); i++ {
		match := true
		for j, w := range words {
			if !items[i+j].lit || items[i+j].text != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func (g litGroup) has(text string) bool {
	return slices.ContainsFunc(g.items, func(it litItem) bool { return it.lit && it.text == text })
}

// toBd reports whether the group is plainly a bd's: called by a function whose
// name says bd, or naming bd itself. A verb of one word that dolt has too, the
// listing of a dolt's own backups for one, is only counted when it is.
func (g litGroup) toBd() bool {
	return strings.Contains(strings.ToLower(g.callee), "bd") || g.has("bd")
}

func verbText(words []string) string { return strings.Join(words, " ") }

// verbExpr matches a verb spelled in one string, as a shell would be given it.
func verbExpr(tool string, words []string) *regexp.Regexp {
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`\b` + tool + `\s+` + strings.Join(parts, `\s+`) + `\b`)
}

func groupHits(fset *token.FileSet, g litGroup) []literalHit {
	line := fset.Position(g.pos).Line
	var hits []literalHit
	var verbs []string
	for _, w := range doltcli.DeniedDoltVerbs {
		if hasRun(g.items, w) {
			verbs = append(verbs, verbText(w))
		}
	}
	for _, w := range doltcli.DeniedBdVerbs {
		if len(w) == 1 && !g.toBd() {
			continue
		}
		if hasRun(g.items, w) {
			verbs = append(verbs, "bd "+verbText(w))
		}
	}
	if len(verbs) > 0 {
		hits = append(hits, literalHit{famVerb, line, strings.Join(verbs, ", ")})
	}
	if g.has(migrateWord) && g.has(forceFlag) {
		hits = append(hits, literalHit{famForce, line, migrateWord + " " + forceFlag})
	}
	return hits
}

// scanLiterals lists every place f spells a guarded text. The names are looked for
// in the raw text, comments included; the verbs and the force flag in the syntax:
// as arguments that follow one another in a call or a composite literal, and as
// words of one string.
func scanLiterals(fset *token.FileSet, f *ast.File, src []byte) []literalHit {
	var hits []literalHit
	for i, line := range strings.Split(string(src), "\n") {
		for _, name := range overrideNames {
			if strings.Contains(line, name) {
				hits = append(hits, literalHit{famName, i + 1, name})
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			hits = append(hits, groupHits(fset, litGroup{n.Pos(), calleeName(n.Fun), itemsOf(n.Args)})...)
		case *ast.CompositeLit:
			hits = append(hits, groupHits(fset, litGroup{n.Pos(), "", itemsOf(n.Elts)})...)
		case *ast.BasicLit:
			text, ok := stringLit(n)
			if !ok {
				return true
			}
			line := fset.Position(n.Pos()).Line
			for _, w := range doltcli.DeniedDoltVerbs {
				if verbExpr("dolt", w).MatchString(text) {
					hits = append(hits, literalHit{famVerb, line, "dolt " + verbText(w)})
				}
			}
			for _, w := range doltcli.DeniedBdVerbs {
				if verbExpr("bd", w).MatchString(text) {
					hits = append(hits, literalHit{famVerb, line, "bd " + verbText(w)})
				}
			}
			if strings.Contains(text, forceFlag) && regexp.MustCompile(`\b`+migrateWord+`\b`).MatchString(text) {
				hits = append(hits, literalHit{famForce, line, migrateWord + " " + forceFlag})
			}
		}
		return true
	})
	return hits
}

func TestB8NoOverrideLiterals(t *testing.T) {
	root, files := harnessFiles(t)
	fset := token.NewFileSet()
	found := map[string]map[string][]string{} // kind -> file -> "line: text"
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.rel)))
		if err != nil {
			t.Fatalf("reading %s: %v", f.rel, err)
		}
		for _, h := range scanLiterals(fset, parseFile(t, fset, root, f.rel), src) {
			if found[h.family] == nil {
				found[h.family] = map[string][]string{}
			}
			found[h.family][f.rel] = append(found[h.family][f.rel], fmt.Sprintf("line %d: %s", h.line, h.text))
		}
	}
	for _, fam := range []string{famName, famVerb, famForce} {
		for _, file := range slices.Sorted(maps.Keys(found[fam])) {
			if _, ok := literalAllowed[fam][file]; !ok {
				t.Errorf("%s: a %s, outside the files that own or test it:\n  %s", file, fam, strings.Join(found[fam][file], "\n  "))
			}
		}
		for _, file := range slices.Sorted(maps.Keys(literalAllowed[fam])) {
			if _, ok := found[fam][file]; !ok {
				t.Errorf("%s is listed as holding a %s (%s) and holds none: drop the entry", file, fam, literalAllowed[fam][file])
			}
		}
	}
}

// B8.NoOverrideLiterals, the other half: the names are not only kept out of the
// source, they are stripped. The one shared list is what does it.
func TestB8OverrideNamesAreStrippedByTheSharedList(t *testing.T) {
	var env []string
	for _, name := range overrideNames {
		env = append(env, name+"=1")
	}
	env = append(env, "KEPT_BY_THE_LIST=1")
	got := doltcli.SanitizedEnv(env)
	for _, kv := range got {
		if name, _, _ := strings.Cut(kv, "="); slices.Contains(overrideNames, name) {
			t.Errorf("%s survived the shared deny-list", name)
		}
	}
	if !slices.Contains(got, "KEPT_BY_THE_LIST=1") {
		t.Errorf("the shared deny-list dropped a variable it has no reason to: %q", got)
	}
}

// ---- the scanner, against sources whose answer is known ----------------------------

func scanSource(t *testing.T, src string) []literalHit {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %q: %v", src, err)
	}
	return scanLiterals(fset, f, []byte(src))
}

func inFunc(stmt string) string { return "package p\n\nfunc f() {\n\t" + stmt + "\n}\n" }

func quoteAll(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = strconv.Quote(w)
	}
	return strings.Join(quoted, ", ")
}

func familiesOf(hits []literalHit) []string {
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.family] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// B8.NoOverrideLiterals can only be trusted if it sees what it is there for, and
// leaves alone what the harness itself does. This feeds it sources whose answer
// is known, built from the shared lists, so a verb added to a list is seen without
// a line of this file changing.
func TestB8LiteralScannerSeesWhatItGuards(t *testing.T) {
	expect := func(t *testing.T, label, src string, want ...string) {
		t.Helper()
		if got := familiesOf(scanSource(t, src)); !slices.Equal(got, want) {
			t.Errorf("%s: found %q in\n%s\nwant %q", label, got, src, want)
		}
	}
	deniedForDolt := func(words []string) bool {
		return slices.ContainsFunc(doltcli.DeniedDoltVerbs, func(d []string) bool { return slices.Equal(d, words) })
	}

	if len(doltcli.DeniedDoltVerbs) == 0 || len(doltcli.DeniedBdVerbs) == 0 {
		t.Fatalf("a deny-list is empty: %v, %v", doltcli.DeniedDoltVerbs, doltcli.DeniedBdVerbs)
	}
	for _, v := range doltcli.DeniedDoltVerbs {
		label := verbText(v)
		expect(t, "dolt "+label+", as arguments", inFunc("runDolt(t, dir, "+quoteAll(v)+", name)"), famVerb)
		expect(t, "dolt "+label+", after the program's name", inFunc("exec.Command(bin, "+quoteAll(append([]string{"dolt"}, v...))+")"), famVerb)
		expect(t, "dolt "+label+", in a list", inFunc("args := []string{"+quoteAll(v)+"}"), famVerb)
		expect(t, "dolt "+label+", in a string", inFunc("sh("+strconv.Quote("dolt "+label+" origin")+")"), famVerb)
	}
	for _, v := range doltcli.DeniedBdVerbs {
		label := verbText(v)
		expect(t, "bd "+label+", as arguments", inFunc("runBd(t, dir, "+quoteAll(v)+", name)"), famVerb)
		expect(t, "bd "+label+", after the program's name", inFunc("exec.Command(bin, "+quoteAll(append([]string{"bd"}, v...))+")"), famVerb)
		expect(t, "bd "+label+", in a string", inFunc("sh("+strconv.Quote("bd "+label+" now")+")"), famVerb)
		if len(v) == 1 && !deniedForDolt(v) {
			// The word is a verb of bd's and a name dolt uses for its own listing.
			expect(t, "dolt's own use of bd's verb "+label, inFunc("runDolt(t, dir, "+quoteAll([]string{v[0], "-v"})+")"))
		}
	}
	// What the seeding does to take a copy away from where it came from is not on
	// the lists, and is not seen.
	for _, args := range [][]string{{"remote", "-v"}, {"remote", "remove"}, {"backup", "-v"}, {"backup", "remove"}} {
		expect(t, "dolt "+verbText(args), inFunc("r.Dolt(ctx, dir, "+quoteAll(args)+", name)"))
	}

	for _, name := range overrideNames {
		expect(t, name+" in a string", inFunc("env = append(env, "+strconv.Quote(name+"=1")+")"), famName)
		expect(t, name+" in a comment", "package p\n\n// "+name+" is set here\n", famName)
	}
	expect(t, "an unrelated variable", inFunc("env = append(env, "+strconv.Quote("BD_ACTOR=x")+")"))

	expect(t, "force on migrate, as arguments", inFunc("runBd(t, dir, "+quoteAll([]string{migrateWord, "schema", forceFlag})+")"), famForce)
	expect(t, "force on migrate, apart", inFunc("runBd(t, dir, "+quoteAll([]string{forceFlag, "--json", migrateWord})+")"), famForce)
	expect(t, "force on migrate, in a list", inFunc("args := []string{"+quoteAll([]string{migrateWord, forceFlag})+"}"), famForce)
	expect(t, "force on migrate, in a string", inFunc("sh("+strconv.Quote("bd "+migrateWord+" schema "+forceFlag)+")"), famForce)
	expect(t, "force on another verb", inFunc("runBd(t, dir, "+quoteAll([]string{"delete", forceFlag, "--", "x"})+")"))
	expect(t, "migrate without force", inFunc("runBd(t, dir, "+quoteAll([]string{migrateWord, "schema", "--json"})+")"))
}
