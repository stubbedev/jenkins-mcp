package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	msg := "git " + strings.Join(e.args, " ")
	if e.stderr != "" {
		return msg + ": " + e.stderr
	}
	return msg + ": " + e.err.Error()
}

func (e *gitError) Unwrap() error { return e.err }

func runGit(ctx context.Context, args ...string) (string, error) {
	full := args
	if dir := cwdFromContext(ctx); dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if cctx.Err() != nil {
			err = fmt.Errorf("timed out after %s: %w", gitTimeout, cctx.Err())
		}
		return "", &gitError{args: full, stderr: strings.TrimSpace(stderr.String()), err: err}
	}
	return strings.TrimSpace(string(out)), nil
}

func isNotARepo(err error) bool {
	var ge *gitError
	return errors.As(err, &ge) && strings.Contains(ge.stderr, "not a git repository")
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

func currentGitRemote(ctx context.Context) (string, error) {
	remote, err := runGit(ctx, "remote", "get-url", "origin")
	var ge *gitError
	switch {
	case err == nil:
		return remote, nil
	case isNotARepo(err), errors.As(err, &ge) && strings.Contains(ge.stderr, "No such remote"):
		return "", nil
	}
	return "", err
}

func currentGitBranch(ctx context.Context) (string, error) {
	return runGit(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}

func currentGitSha(ctx context.Context) (string, error) {
	return runGit(ctx, "rev-parse", "HEAD")
}

// logErr writes a line to stderr (stdout is reserved for the MCP stdio stream).
func logErr(msg string) { os.Stderr.WriteString(msg + "\n") }
