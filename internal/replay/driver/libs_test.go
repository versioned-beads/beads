package driver

import (
	"context"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// seededDB returns a dolt database whose issues table holds one row with the
// given title, and the commit that holds it.
func seededDB(t *testing.T, name, title string) (dir, head string) {
	t.Helper()
	dir = replaytest.NewDoltDB(t, name)
	replaytest.RunDolt(t, dir, "sql", "-q", "CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, title VARCHAR(255), status VARCHAR(32))")
	replaytest.RunDolt(t, dir, "sql", "-q", "INSERT INTO issues VALUES ('x-1', '"+title+"', 'open')")
	replaytest.RunDolt(t, dir, "add", "-A")
	replaytest.RunDolt(t, dir, "commit", "-m", "seed")
	return dir, replaytest.HeadCommit(t, dir)
}

// B1.DriverUsesLibs: the driver reads views through oracle.ReadView and compares
// them with compare.CompareViews, in process. The step environment of a real run
// hands back the library's own View, equal to what a direct read returns, and
// the comparison it feeds is the library's, so the verdict equals what a direct
// call produces for the same two views.
func TestB1DriverUsesLibs(t *testing.T) {
	replaytest.Require(t, replaytest.NeedDolt)
	ctx := context.Background()

	oracleDir, oracleHead := seededDB(t, "oracle", "Widget")
	sameDir, sameHead := seededDB(t, "same", "Widget")
	otherDir, otherHead := seededDB(t, "other", "Gadget")

	cases := []struct {
		name      string
		dir, head string
		wantMatch bool
	}{
		{"identical rows match", sameDir, sameHead, true},
		{"different title mismatches", otherDir, otherHead, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := realEnv{cfg: RunConfig{OracleDataDir: oracleDir, WorkDataDir: tc.dir}}

			oracleView, err := env.OracleView(ctx, oracleHead, "x-1")
			if err != nil {
				t.Fatalf("OracleView: %v", err)
			}
			state, err := b11aReaderNamed(t, env.Readers(), "row").Read(ctx, tc.dir, "x-1", tc.head)
			if err != nil {
				t.Fatalf("the row reader: %v", err)
			}
			candidateView := state.View
			var _ *oracle.View = oracleView // the driver hands back the library's type

			direct, err := oracle.ReadView(ctx, oracleDir, oracleHead, "x-1")
			if err != nil {
				t.Fatalf("oracle.ReadView: %v", err)
			}
			if !reflect.DeepEqual(oracleView, direct) {
				t.Errorf("the driver's view differs from the library's:\n got  %+v\n want %+v", oracleView, direct)
			}

			got, _, err := compare.CompareViews(oracleView, candidateView)
			if err != nil {
				t.Fatalf("compare.CompareViews: %v", err)
			}
			if got.Matched != tc.wantMatch {
				t.Errorf("Matched = %v, want %v", got.Matched, tc.wantMatch)
			}

			head, err := env.WorkHead(ctx)
			if err != nil {
				t.Fatalf("WorkHead: %v", err)
			}
			if want := replaytest.HeadCommit(t, tc.dir); head != want {
				t.Errorf("WorkHead = %s, want the database's head %s", head, want)
			}
		})
	}
}
