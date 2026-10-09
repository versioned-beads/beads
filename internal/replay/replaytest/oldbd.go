package replaytest

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// OldBdSHA names the commit the older bd is built from: an upstream commit whose
// migrations stop several short of this tree's, so a store it wrote is one the
// current bd has to migrate before it can write. A fixture that needs an oracle
// behind the integration's schema builds it with this bd.
const OldBdSHA = "9cb22b790cb1a7cb1a9af0abc3ec87a1ad1fed2b"

var oldBdBuild struct {
	once   sync.Once
	dir    string
	path   string
	prereq string
	err    error
}

// OldBd returns the bd built from OldBdSHA, once per test binary, from that
// commit's own source. It is never taken from PATH. A checkout that lacks the
// commit (a shallow clone), or a machine without git, cannot build it: that is a
// missing prerequisite, a failure in a lane that sets REPLAY_REQUIRE=1 and a skip
// anywhere else. Call Main from TestMain to remove the build when the tests
// finish.
func OldBd(t testing.TB) string {
	t.Helper()
	Require(t, NeedBd)
	oldBdBuild.once.Do(func() { buildOldBd(bazeltest.RepoRoot(t)) })
	if oldBdBuild.prereq != "" {
		msg := oldBdBuild.prereq
		if os.Getenv(requireEnv) == "1" {
			t.Fatalf("%s, and %s=1 says this lane requires it", msg, requireEnv)
		}
		t.Skipf("%s; set %s=1 to fail instead of skipping", msg, requireEnv)
	}
	if oldBdBuild.err != nil {
		t.Fatalf("%v", oldBdBuild.err)
	}
	return oldBdBuild.path
}

func buildOldBd(root string) {
	if _, err := exec.LookPath("git"); err != nil {
		oldBdBuild.prereq = "building the older bd needs git on PATH"
		return
	}
	if out, err := exec.Command("git", "-C", root, "cat-file", "-e", OldBdSHA+"^{commit}").CombinedOutput(); err != nil {
		oldBdBuild.prereq = fmt.Sprintf("commit %s is not in this checkout (a shallow clone drops it: fetch the full history): %v: %s", OldBdSHA, err, bytes.TrimSpace(out))
		return
	}
	dir, err := os.MkdirTemp("", "replaytest-oldbd-")
	if err != nil {
		oldBdBuild.err = err
		return
	}
	oldBdBuild.dir = dir
	src := filepath.Join(dir, "src")
	if err := extractCommit(root, OldBdSHA, src); err != nil {
		oldBdBuild.err = fmt.Errorf("extracting %s: %w", OldBdSHA, err)
		return
	}
	out := filepath.Join(dir, "bd")
	cmd := exec.Command("go", "build", "-tags", bdBuildTags, "-o", out, "./cmd/bd")
	cmd.Dir = src
	cmd.Env = baseEnv
	if b, err := cmd.CombinedOutput(); err != nil {
		oldBdBuild.err = fmt.Errorf("building bd from %s: %w\n%s", OldBdSHA, err, b)
		return
	}
	oldBdBuild.path = out
}

// removeOldBd deletes the older bd's source and binary, if a test built them.
func removeOldBd() {
	if oldBdBuild.dir != "" {
		_ = os.RemoveAll(oldBdBuild.dir)
	}
}

// extractCommit writes the tree of commit sha, as git archive reports it, into
// dst. Only directories and regular files are written: symlinks and the
// archive's own header carry nothing the build reads.
func extractCommit(root, sha, dst string) error {
	cmd := exec.Command("git", "-C", root, "archive", "--format=tar", sha)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := untar(tar.NewReader(stdout), dst); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func untar(tr *tar.Reader, dst string) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !filepath.IsLocal(hdr.Name) {
			return fmt.Errorf("archive entry %q is outside the destination", hdr.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			perm := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				perm = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) // #nosec G304 -- target is under the extraction directory, checked above
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr) // #nosec G110 -- the archive comes from this checkout's own history
			if err := f.Close(); copyErr == nil {
				copyErr = err
			}
			if copyErr != nil {
				return copyErr
			}
		}
	}
}
