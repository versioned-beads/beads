package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// The parts of the seeding that are decisions and not measurements: which tables
// are cleared, what the project file holds, how a refusal is classed and what is
// recorded as the engine. Most of these need no store.

// ---- B8.IgnoredPlanePolicyDrift ---------------------------------------------------

// B8.IgnoredPlanePolicyDrift: the recipe has a policy for every ignored table the
// product creates, and a table it has no policy for refuses the seeding. The first
// half reads the policy against a store the integration's own bd just made, so a
// new ignored table in the product fails here, in the harness's own tests, and not
// as a refusal in the middle of a real seeding. The second half is the refusal.
func TestB8IgnoredPlanePolicyDrift(t *testing.T) {
	t.Run("covers a fresh store", func(t *testing.T) {
		requireBd(t)
		requireDolt(t)
		dir := initBdProjectWith(t, "drift", replaytest.BdBin(t))
		names := ignoredTables(t, dataDir(t, dir))
		if len(names) == 0 {
			t.Fatalf("a fresh store has no ignored table")
		}
		plan, err := classifyIgnoredPlane(names)
		if err != nil {
			t.Fatalf("the policy refuses a store the integration's bd just made: %v", err)
		}
		var all []string
		for _, list := range [][]string{plan.Clear, plan.Counter, plan.Keep} {
			all = append(all, list...)
		}
		slices.Sort(all)
		if !slices.Equal(all, slices.Compact(slices.Clone(all))) {
			t.Errorf("a table is in more than one list of the policy: %q", all)
		}
		if want := slices.Sorted(slices.Values(names)); !slices.Equal(all, want) {
			t.Errorf("the policy covers %q, the store's ignored tables are %q", all, want)
		}
		for _, name := range plan.Clear {
			if !seedClears(name) {
				t.Errorf("%s is cleared, and the test's own statement of the cleared tables does not name it", name)
			}
		}
		for _, name := range names {
			if seedClears(name) && !slices.Contains(plan.Clear, name) {
				t.Errorf("%s is not cleared, and the test's own statement of the cleared tables names it", name)
			}
		}
		if !slices.Equal(plan.Counter, []string{"bd_events_seq"}) {
			t.Errorf("the counter tables are %q, want only bd_events_seq", plan.Counter)
		}
		if !slices.Equal(plan.Keep, []string{"ignored_schema_migrations"}) {
			t.Errorf("the kept tables are %q, want only ignored_schema_migrations", plan.Keep)
		}
	})
	t.Run("refuses a table it has no policy for", func(t *testing.T) {
		_, err := classifyIgnoredPlane([]string{"events", "wisps", "wisp_labels", "surprise_table"})
		var sr *SeedRefused
		if !errors.As(err, &sr) || sr.Class != RefusedIgnoredPlane {
			t.Fatalf("an unknown ignored table gave %v, want a refusal of class %s", err, RefusedIgnoredPlane)
		}
		if !strings.Contains(sr.Detail, "surprise_table") {
			t.Errorf("the refusal's detail %q does not name the table", sr.Detail)
		}
		if strings.Contains(sr.Detail, "wisp_labels") {
			t.Errorf("the refusal's detail %q names a table the policy covers", sr.Detail)
		}
	})
	t.Run("refuses a name that is not an identifier", func(t *testing.T) {
		for _, name := range []string{"x; DROP TABLE issues", "wisps WHERE 1=1", "", "a-b", "`wisps`"} {
			_, err := classifyIgnoredPlane([]string{"events", name})
			var sr *SeedRefused
			if !errors.As(err, &sr) || sr.Class != RefusedIgnoredPlane || sr.Detail == "" {
				t.Errorf("the name %q gave %v, want a refusal of class %s with a detail", name, err, RefusedIgnoredPlane)
			}
		}
	})
	t.Run("names a table once", func(t *testing.T) {
		plan, err := classifyIgnoredPlane([]string{"wisps", "wisps", "events", "bd_events_seq", "bd_events_seq"})
		if err != nil {
			t.Fatalf("a repeated name: %v", err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(plan.Clear)), []string{"events", "wisps"}) {
			t.Errorf("the cleared tables are %q, want events and wisps once each", plan.Clear)
		}
		if !slices.Equal(plan.Counter, []string{"bd_events_seq"}) {
			t.Errorf("the counter tables are %q, want bd_events_seq once", plan.Counter)
		}
	})
}

// ---- B8.IdentityWrittenNotOmitted, the file's text --------------------------------

// seedJSONKeys is the keys of a flat JSON object in the order the file lists them.
func seedJSONKeys(t testing.TB, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object (%v, %v):\n%s", tok, err, data)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("reading a key: %v\n%s", err, data)
		}
		name, ok := k.(string)
		if !ok {
			t.Fatalf("a key that is not a string: %v", k)
		}
		keys = append(keys, name)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("reading the value of %s: %v\n%s", name, err, data)
		}
	}
	return keys
}

// B8.IdentityWrittenNotOmitted: the project file has the keys and the order of a
// fresh init's, so that a product that reads the file reads a copy's as it reads
// any other, and it carries the identity when there is one and no key for it when
// there is not.
func TestB8SeedMetadataIsTheFreshInitShape(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	fresh, err := os.ReadFile(filepath.Join(templateProject(t), ".beads", "metadata.json")) // #nosec G304 -- the template this test binary made
	if err != nil {
		t.Fatalf("reading a fresh init's project file: %v", err)
	}
	var freshMeta map[string]any
	if err := json.Unmarshal(fresh, &freshMeta); err != nil {
		t.Fatalf("a fresh init's project file: %v\n%s", err, fresh)
	}

	got, err := renderSeedMetadata("oracle", "an-identity")
	if err != nil {
		t.Fatalf("rendering the project file: %v", err)
	}
	if want := seedJSONKeys(t, fresh); !slices.Equal(seedJSONKeys(t, got), want) {
		t.Errorf("the keys are %q, a fresh init's are %q", seedJSONKeys(t, got), want)
	}
	var meta map[string]any
	if err := json.Unmarshal(got, &meta); err != nil {
		t.Fatalf("the rendered project file: %v\n%s", err, got)
	}
	for _, k := range []string{"database", "backend", "dolt_mode"} {
		if meta[k] != freshMeta[k] {
			t.Errorf("%s is %v, a fresh init's is %v", k, meta[k], freshMeta[k])
		}
	}
	if meta["dolt_database"] != "oracle" || meta["project_id"] != "an-identity" {
		t.Errorf("dolt_database and project_id are %v and %v, want oracle and an-identity", meta["dolt_database"], meta["project_id"])
	}
	if !bytes.Contains(got, []byte("\n  \"database\": ")) {
		t.Errorf("the file is not indented by two spaces:\n%s", got)
	}

	none, err := renderSeedMetadata("oracle", "")
	if err != nil {
		t.Fatalf("rendering a project file without an identity: %v", err)
	}
	var noneMeta map[string]any
	if err := json.Unmarshal(none, &noneMeta); err != nil {
		t.Fatalf("the rendered project file: %v\n%s", err, none)
	}
	if _, ok := noneMeta["project_id"]; ok || len(noneMeta) != 4 {
		t.Errorf("a project file without an identity has %v, want the other four keys and no project_id", noneMeta)
	}
}

// ---- B8.LinkedEngineNamed ---------------------------------------------------------

// B8.LinkedEngineNamed: the record names the dolt engine the bd under test links,
// read from go.mod's text and not from a binary, which a build system other than
// go may leave with nothing to read.
func TestB8LinkedEngineNamed(t *testing.T) {
	t.Run("this tree", func(t *testing.T) {
		goMod, err := os.ReadFile(filepath.Join(bazeltest.RepoRoot(t), "go.mod")) // #nosec G304 -- this tree's own go.mod
		if err != nil {
			t.Fatalf("reading go.mod: %v", err)
		}
		got, err := LinkedEngine(goMod)
		if err != nil {
			t.Fatalf("LinkedEngine on this tree's go.mod: %v", err)
		}
		if want := repoLinkedEngine(t); got != want {
			t.Errorf("LinkedEngine = %q, want %q", got, want)
		}
	})
	const engine = "github.com/dolthub/dolt/go"
	for _, c := range []struct {
		name  string
		goMod string
		want  string
	}{
		{
			"block",
			"module example.test/m\n\ngo 1.26\n\nrequire (\n\tgithub.com/other/mod v1.0.0\n\t" + engine + " v0.40.5-0.20260806213044-796d07497741\n)\n",
			engine + " v0.40.5-0.20260806213044-796d07497741",
		},
		{
			"single line",
			"module example.test/m\n\nrequire " + engine + " v1.2.3\nrequire github.com/other/mod v1.0.0\n",
			engine + " v1.2.3",
		},
		{
			"indirect",
			"module example.test/m\n\nrequire (\n\t" + engine + " v1.2.3 // indirect\n)\n",
			engine + " v1.2.3",
		},
		{
			"not a prefix of another module",
			"module example.test/m\n\nrequire (\n\t" + engine + "/extra v9.9.9\n\t" + engine + " v1.2.3\n)\n",
			engine + " v1.2.3",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := LinkedEngine([]byte(c.goMod))
			if err != nil || got != c.want {
				t.Errorf("LinkedEngine = %q, %v, want %q", got, err, c.want)
			}
		})
	}
	t.Run("replaced", func(t *testing.T) {
		goMod := "module example.test/m\n\nrequire " + engine + " v1.2.3\n\nreplace " + engine + " => example.test/fork/dolt/go v1.0.0\n"
		got, err := LinkedEngine([]byte(goMod))
		if err != nil {
			return // refusing a replaced engine is an acceptable answer
		}
		if !strings.Contains(got, "=>") || !strings.Contains(got, "example.test/fork/dolt/go") {
			t.Errorf("LinkedEngine = %q for a replaced engine, want the replacement named or an error", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		if got, err := LinkedEngine([]byte("module example.test/m\n\nrequire github.com/other/mod v1.0.0\n")); err == nil {
			t.Errorf("LinkedEngine = %q for a go.mod without the engine, want an error", got)
		}
		if got, err := LinkedEngine(nil); err == nil {
			t.Errorf("LinkedEngine = %q for no go.mod, want an error", got)
		}
	})
}

// ---- B8.RefusalClasses ------------------------------------------------------------

// B8.RefusalClasses (H18): every class of refusal but one is a fault of the
// harness's own recipe or fixture. The migration verb failing on a copy the recipe
// prepared correctly is a finding about the product, and is the one class that is
// not a fault of the harness.
func TestB8RefusalClasses(t *testing.T) {
	for class, fault := range map[string]bool{
		RefusedBaseSchema:        true,
		RefusedIgnoredPlane:      true,
		RefusedRemotes:           true,
		RefusedBackups:           true,
		RefusedIdentity:          true,
		RefusedRemoteMigrateGate: true,
		RefusedMigration:         false,
	} {
		if got := (&SeedRefused{Class: class}).HarnessFault(); got != fault {
			t.Errorf("a refusal of class %s: HarnessFault = %v, want %v", class, got, fault)
		}
	}
	e := &SeedRefused{Class: RefusedBackups, Detail: "1 backup remains"}
	if got, want := e.Error(), "seed-refused(backups): 1 backup remains"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var target *SeedRefused
	if !errors.As(error(e), &target) || target != e {
		t.Errorf("a refusal is not found by errors.As")
	}
}

// ---- the dump tooling -------------------------------------------------------------

// B8.SeedChildEnvIsolated and B8.OverrideEnvStripped read what the stand-ins wrote
// down. This checks the stand-in and the parser against a tool whose environment
// is known: it must record exactly what the process was given, under a PATH that
// holds nothing, keep the order of the children, and pass the arguments and the
// exit status through. It tests the tooling and not the seeding, and so it passes
// whatever the seeding does.
func TestB8DumpToolingSelfCheck(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skipf("echo: %v", err)
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "children.txt")
	first := seedWriteStandIn(t, dir, "first", echo, dump)
	second := seedWriteStandIn(t, dir, "second", echo, dump)

	run := func(bin string, env []string, args ...string) string {
		cmd := exec.Command(bin, args...) // #nosec G204 -- a stand-in this test wrote
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", bin, err, out)
		}
		return string(out)
	}
	bogus := filepath.Join(dir, "nowhere")
	if got := run(first, []string{"ALPHA=1", "BETA=two words", "EMPTY=", "PATH=" + bogus, "MULTI=line one\nline two"}, "a", "b"); got != "a b\n" {
		t.Errorf("the stand-in passed %q through, want the real tool's output for the same arguments", got)
	}
	run(second, []string{"GAMMA=3"})

	blocks := parseSeedDump(t, dump)
	if len(blocks) != 2 || blocks[0].label != "first" || blocks[1].label != "second" {
		t.Fatalf("the dump holds %+v, want the two children in order", blocks)
	}
	env := seedWithoutShellNames(blocks[0].env)
	if env["ALPHA"] != "1" || env["BETA"] != "two words" || env["PATH"] != bogus {
		t.Errorf("the first child's environment was recorded as %v", env)
	}
	if v, ok := env["EMPTY"]; !ok || v != "" {
		t.Errorf("an empty variable was recorded as %q (present %v)", v, ok)
	}
	if !strings.HasPrefix(env["MULTI"], "line one") {
		t.Errorf("a variable that holds a newline was recorded as %q", env["MULTI"])
	}
	if _, ok := env["line two"]; ok || len(env) != 5 {
		t.Errorf("the first child's environment holds %v, want its five variables and nothing the value's second line made", env)
	}
	if got := seedWithoutShellNames(blocks[1].env); len(got) != 1 || got["GAMMA"] != "3" {
		t.Errorf("the second child's environment was recorded as %v, want GAMMA alone", got)
	}
}
