package versions

import (
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/replay/replaytest"
)

func TestMain(m *testing.M) {
	os.Exit(replaytest.Main(m))
}
