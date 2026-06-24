package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds every git invocation so a hung command (e.g. a credential
// prompt on a misconfigured repo) can never block a tool call indefinitely.
const gitTimeout = 10 * time.Second

// cwdKey carries the working directory in which git commands should run. In
// stdio mode it is unset and git runs in the server's own cwd. In HTTP mode the
// harness (or a fronting proxy) supplies it per request — via the X-Repo-Root /
// X-Cwd header or a `cwd` tool argument — so git resolves the caller's repo
// rather than the server's.
type cwdKey struct{}

// withCwd returns a context carrying dir as the git working directory. Empty
// dir is ignored so callers can pass through unconditionally.
func withCwd(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, cwdKey{}, dir)
}

// cwdFromContext returns the git working directory carried by ctx, or "".
func cwdFromContext(ctx context.Context) string {
	if dir, ok := ctx.Value(cwdKey{}).(string); ok {
		return dir
	}
	return ""
}

// safeGit runs git with the given args, returning trimmed stdout, or "" on any
// error (stderr is discarded). When the context carries a cwd, git runs there
// (git -C <cwd>); otherwise it runs in the server's current working directory.
func safeGit(ctx context.Context, args ...string) string {
	if dir := cwdFromContext(ctx); dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	// GIT_TERMINAL_PROMPT=0 makes git fail fast instead of blocking on an
	// interactive credential/host prompt — belt-and-suspenders with the timeout.
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isRepoDir reports whether dir is an absolute path to an existing directory —
// a caller-supplied working tree must be both before git can run there. A
// relative path is meaningless to a server process whose cwd the caller cannot
// see, so it is rejected.
func isRepoDir(dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func currentGitRemote(ctx context.Context) string {
	return safeGit(ctx, "remote", "get-url", "origin")
}
func currentGitBranch(ctx context.Context) string {
	return safeGit(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}
func currentGitSha(ctx context.Context) string { return safeGit(ctx, "rev-parse", "HEAD") }

// logErr writes a line to stderr (stdout is reserved for the MCP stdio stream).
func logErr(msg string) { os.Stderr.WriteString(msg + "\n") }
