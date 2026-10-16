package main

import (
	"context"
	"strings"
	"testing"
)

func TestCoerceBuildRef(t *testing.T) {
	cases := []struct {
		in      any
		want    string
		wantErr bool
	}{
		{nil, "", false},
		{float64(42), "42", false},
		{"42", "42", false},
		{"", "", false},
		{"lastBuild", "lastBuild", false},
		{"lastSuccessfulBuild", "lastSuccessfulBuild", false},
		{"garbage", "", true},
		{true, "", true},
	}
	for _, c := range cases {
		got, err := coerceBuildRef(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("coerceBuildRef(%v): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("coerceBuildRef(%v) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestCoerceBuildNumber(t *testing.T) {
	if n, err := coerceBuildNumber(float64(7)); err != nil || n != 7 {
		t.Errorf("coerceBuildNumber(7.0) = %d, %v", n, err)
	}
	if n, err := coerceBuildNumber("12"); err != nil || n != 12 {
		t.Errorf("coerceBuildNumber(\"12\") = %d, %v", n, err)
	}
	if _, err := coerceBuildNumber("lastBuild"); err == nil {
		t.Error("coerceBuildNumber(lastBuild): want error")
	}
	if _, err := coerceBuildNumber(nil); err == nil {
		t.Error("coerceBuildNumber(nil): want error")
	}
}

func TestNormalizeArgsJobAlias(t *testing.T) {
	args := normalizeArgs(map[string]any{"job": "folder/x"})
	if args["jobPath"] != "folder/x" {
		t.Errorf("job alias not applied: %v", args)
	}
	// explicit jobPath wins over job
	args = normalizeArgs(map[string]any{"job": "a", "jobPath": "b"})
	if args["jobPath"] != "b" {
		t.Errorf("jobPath should win: %v", args)
	}
}

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
	defaultFormat = "toon"
	if got := pickFormat(map[string]any{"format": "json"}); got != "json" {
		t.Errorf("explicit json: %q", got)
	}
	if got := pickFormat(map[string]any{"format": "bogus"}); got != "toon" {
		t.Errorf("bogus falls back to default: %q", got)
	}
	if got := pickFormat(map[string]any{}); got != "toon" {
		t.Errorf("no arg uses default: %q", got)
	}
}

func TestRenderStructured(t *testing.T) {
	v := map[string]any{"name": "x", "n": 3}
	j := renderStructured(v, "json")
	if !strings.Contains(j, "\"name\": \"x\"") {
		t.Errorf("json render missing field: %s", j)
	}
	// TOON should render numbers unquoted and be non-empty
	tn := renderStructured(v, "toon")
	if tn == "" || strings.Contains(tn, "\"n\": \"3\"") {
		t.Errorf("toon render looks wrong: %s", tn)
	}
}

func TestResolveRevision(t *testing.T) {
	dir := t.TempDir()
	ctx := withCwd(context.Background(), dir)
	git := func(args ...string) string {
		t.Helper()
		out := safeGit(ctx, args...)
		if out == "" {
			t.Skipf("git %v produced no output (git missing?)", args)
		}
		return out
	}
	git("init", "-b", "feature/x")
	safeGit(ctx, "remote", "add", "origin", "git@host:team/repo.git")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "one")
	first := git("rev-parse", "HEAD")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "two")
	head := git("rev-parse", "HEAD")

	remote, branch, sha, err := resolveRevision(ctx, "", "", "")
	if err != nil || remote != "git@host:team/repo.git" || branch != "feature/x" || sha != head {
		t.Errorf("defaults = %q %q %q %v; want origin, feature/x, %s", remote, branch, sha, err, head)
	}

	_, branch, sha, err = resolveRevision(ctx, "", "", first[:8])
	if err != nil || branch != "" || sha != first {
		t.Errorf("short sha = %q %q %v; want no branch and %s", branch, sha, err, first)
	}

	_, branch, sha, err = resolveRevision(ctx, "https://other/x.git", "main", "HEAD~1")
	if err != nil || branch != "main" || sha != first {
		t.Errorf("explicit = %q %q %v", branch, sha, err)
	}

	upper := strings.ToUpper(head)
	if _, _, sha, err = resolveRevision(context.Background(), "u", "", upper); err != nil || sha != head {
		t.Errorf("full sha without a repo = %q %v", sha, err)
	}

	if _, _, _, err = resolveRevision(ctx, "", "", "--output=/tmp/x"); err == nil {
		t.Error("want error for an unresolvable revision")
	}
}
