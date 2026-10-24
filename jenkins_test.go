package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestJobPathToAPISegments(t *testing.T) {
	if got := jobPathToAPISegments("platform/api/build master"); got != "job/platform/job/api/job/build%20master" {
		t.Errorf("segments = %q", got)
	}
	if got := jobPathToAPISegments("/leading//double/"); got != "job/leading/job/double" {
		t.Errorf("segments = %q", got)
	}
}

func TestShaPrefixMatches(t *testing.T) {
	if !shaPrefixMatches("abc1234", "ABC1234def") {
		t.Error("7-char prefix should match case-insensitively")
	}
	if shaPrefixMatches("abc12", "abc12def") {
		t.Error("under 7 chars must not match")
	}
}

func TestFmtDuration(t *testing.T) {
	cases := map[int64]string{
		0:       "?",
		-5:      "?",
		5000:    "5s",
		65000:   "1m5s",
		120000:  "2m",
		3660000: "1h1m",
		3600000: "1h",
	}
	for ms, want := range cases {
		if got := fmtDuration(ms); got != want {
			t.Errorf("fmtDuration(%d) = %q; want %q", ms, got, want)
		}
	}
}

func TestExtractCommitAndBranch(t *testing.T) {
	sha := "deadbeef"
	actions := []buildAction{{
		LastBuiltRevision: &struct {
			SHA1   string `json:"SHA1"`
			Branch []struct {
				SHA1 string `json:"SHA1"`
				Name string `json:"name"`
			} `json:"branch"`
		}{SHA1: sha, Branch: []struct {
			SHA1 string `json:"SHA1"`
			Name string `json:"name"`
		}{{Name: "origin/master"}}},
	}}
	if got := extractCommit(actions); got != sha {
		t.Errorf("extractCommit = %q", got)
	}
	if got := extractBranch(actions); got != "origin/master" {
		t.Errorf("extractBranch = %q", got)
	}
}

func TestPickFormat(t *testing.T) {
	saved := defaultFormat
	t.Cleanup(func() { defaultFormat = saved })
	defaultFormat = formatJSON
	if got, err := pickFormat(toolArgs{values: map[string]any{"format": "toon"}}); err != nil || got != formatTOON {
		t.Errorf("explicit toon: %q, %v", got, err)
	}
	if got, err := pickFormat(toolArgs{values: map[string]any{}}); err != nil || got != formatJSON {
		t.Errorf("absent format must use the process default: %q, %v", got, err)
	}
}

func TestRenderStructured(t *testing.T) {
	v := map[string]any{"name": "x", "n": 3}
	j, err := renderStructured(v, formatJSON)
	if err != nil || !strings.Contains(j, "\"name\": \"x\"") {
		t.Errorf("json render = %q, %v", j, err)
	}
	tn, err := renderStructured(v, formatTOON)
	if err != nil || tn == "" || strings.Contains(tn, "\"n\": \"3\"") {
		t.Errorf("toon render = %q, %v", tn, err)
	}
	for _, format := range []outputFormat{formatJSON, formatTOON} {
		if out, err := renderStructured(map[string]any{"bad": make(chan int)}, format); err == nil {
			t.Errorf("%s: unencodable value rendered as %q instead of failing", format, out)
		}
	}
}

func TestResolveRevision(t *testing.T) {
	dir := t.TempDir()
	ctx := withCwd(context.Background(), dir)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "feature/x")
	git("remote", "add", "origin", "git@host:team/repo.git")
	git("commit", "--allow-empty", "-m", "one")
	first := git("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "two")
	head := git("rev-parse", "HEAD")

	cases := []struct {
		name                string
		ctx                 context.Context
		remote, branch, rev string
		want                gitRevision
	}{
		{"defaults from working tree", ctx, "", "", "", gitRevision{remote: "git@host:team/repo.git", branch: "feature/x", sha: head}},
		{"short sha drops branch filter", ctx, "", "", first[:8], gitRevision{remote: "git@host:team/repo.git", sha: first}},
		{"explicit url, branch and revision", ctx, "https://other/x.git", "main", "HEAD~1", gitRevision{remote: "https://other/x.git", branch: "main", sha: first}},
		{"full sha needs no repo", context.Background(), "u", "", strings.ToUpper(head), gitRevision{remote: "u", sha: head}},
	}
	for _, tc := range cases {
		got, err := resolveRevision(tc.ctx, tc.remote, tc.branch, tc.rev)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}

	for _, tc := range []struct {
		name string
		ctx  context.Context
		rev  string
		want string
	}{
		{"option-shaped revision", ctx, "--output=/tmp/x", "cannot resolve"},
		{"relative cwd", withCwd(context.Background(), "relative/dir"), "", "not an absolute"},
		{"no origin remote", withCwd(context.Background(), t.TempDir()), "", "url is required"},
	} {
		if _, err := resolveRevision(tc.ctx, "", "", tc.rev); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestParsePatchRejectsUnknownFields(t *testing.T) {
	if p, err := parsePatch(map[string]any{"description": "d", "disabled": true}); err != nil || p.cfg.Description == nil || *p.cfg.Description != "d" {
		t.Fatalf("known fields: got %+v, %v", p, err)
	}
	for _, field := range []string{"type", "scm", "otherTriggers", "buildRevision", "descripton"} {
		if _, err := parsePatch(map[string]any{field: "x"}); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("patch with %q: want unknown-field error, got %v", field, err)
		}
	}
}
