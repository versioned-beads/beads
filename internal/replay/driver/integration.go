package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// bdBuildTags is the build tag every bd in this repo is built with. It keeps
// go-mysql-server on Go's own regexp instead of the ICU-backed package, whose C
// headers a macOS runner does not have.
const bdBuildTags = "gms_pure_go"

// cleanupTimeout bounds the removal of a build's worktree, which has to run when
// the build was cut short and its context is done.
const cleanupTimeout = 30 * time.Second

// BuildIntegration resolves ref to an exact commit SHA in repoDir, builds
// pkgPath from that commit's tree into outBin, and returns the resolved SHA.
// The build carries the suite's tags. It happens in a temporary git worktree
// outside repoDir so the caller's own working tree and HEAD are never touched;
// the worktree is removed again before returning, an interrupted build's too.
//
// The SHA is the one commit the build was made from. A caller that records it
// does not resolve ref a second time, which could name another commit if the ref
// moved in between.
func BuildIntegration(ctx context.Context, repoDir, ref, pkgPath, outBin string) (string, error) {
	sha, err := runGit(ctx, repoDir, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("build integration: resolve ref %q: %w", ref, err)
	}

	worktreeDir, err := os.MkdirTemp("", "driver-core-integration-*")
	if err != nil {
		return "", fmt.Errorf("build integration: create worktree dir: %w", err)
	}
	defer removeWorktree(ctx, repoDir, worktreeDir)

	if _, err := runGit(ctx, repoDir, "worktree", "add", "--detach", worktreeDir, sha); err != nil {
		return "", fmt.Errorf("build integration: add worktree for %s: %w", sha, err)
	}

	cmd := exec.CommandContext(ctx, "go", "build", "-tags", bdBuildTags, "-o", outBin, pkgPath)
	cmd.Dir = worktreeDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build integration: go build %s at %s: %w\n%s", pkgPath, sha, err, out)
	}

	return sha, nil
}

// removeWorktree unregisters the worktree at dir from repoDir and removes it. It
// runs when the build is over however it ended, an interrupt included, so it does
// not run under ctx, which is done by then: a command given a done context never
// starts, and the worktree would stay registered in the caller's repository. A
// directory that is not a registered worktree, because adding it never finished,
// is only removed.
func removeWorktree(ctx context.Context, repoDir, dir string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_, _ = runGit(ctx, repoDir, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
}

// runGit runs a git subcommand in dir and returns its trimmed stdout.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
