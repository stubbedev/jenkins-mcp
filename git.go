package main

import (
	"os"
	"os/exec"
	"strings"
)

// safeGit runs git with the given args in the current working directory and
// returns trimmed stdout, or "" on any error (stderr is discarded).
func safeGit(args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func currentGitRemote() string { return safeGit("remote", "get-url", "origin") }
func currentGitBranch() string { return safeGit("rev-parse", "--abbrev-ref", "HEAD") }
func currentGitSha() string    { return safeGit("rev-parse", "HEAD") }

// logErr writes a line to stderr (stdout is reserved for the MCP stdio stream).
func logErr(msg string) { os.Stderr.WriteString(msg + "\n") }
