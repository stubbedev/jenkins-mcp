package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTransport(t *testing.T) {
	// Env must not leak between cases.
	t.Setenv("JENKINS_MCP_HTTP", "")
	t.Setenv("JENKINS_MCP_HTTP_PATH", "")

	cases := []struct {
		name          string
		env           map[string]string
		args          []string
		wantHTTP      bool
		wantAddr      string
		wantPath      string
		wantStateless bool
	}{
		{
			name:     "default stdio",
			args:     nil,
			wantHTTP: false,
			wantAddr: "127.0.0.1:8080",
			wantPath: "/mcp",
		},
		{
			name:     "flag bare enables http on default addr",
			args:     []string{"--http"},
			wantHTTP: true,
			wantAddr: "127.0.0.1:8080",
			wantPath: "/mcp",
		},
		{
			name:     "flag with separate addr",
			args:     []string{"--http", "0.0.0.0:9000"},
			wantHTTP: true,
			wantAddr: "0.0.0.0:9000",
			wantPath: "/mcp",
		},
		{
			name:     "flag with =addr",
			args:     []string{"--http=:9100"},
			wantHTTP: true,
			wantAddr: ":9100",
			wantPath: "/mcp",
		},
		{
			name:          "flag does not eat following flag",
			args:          []string{"--http", "--http-stateless"},
			wantHTTP:      true,
			wantAddr:      "127.0.0.1:8080",
			wantPath:      "/mcp",
			wantStateless: true,
		},
		{
			name:     "env false keeps stdio",
			env:      map[string]string{"JENKINS_MCP_HTTP": "false"},
			wantHTTP: false,
			wantAddr: "127.0.0.1:8080",
			wantPath: "/mcp",
		},
		{
			name:     "env 0 keeps stdio",
			env:      map[string]string{"JENKINS_MCP_HTTP": "0"},
			wantHTTP: false,
			wantAddr: "127.0.0.1:8080",
			wantPath: "/mcp",
		},
		{
			name:     "env addr",
			env:      map[string]string{"JENKINS_MCP_HTTP": "1.2.3.4:7000"},
			wantHTTP: true,
			wantAddr: "1.2.3.4:7000",
			wantPath: "/mcp",
		},
		{
			name:     "env truthy uses default addr",
			env:      map[string]string{"JENKINS_MCP_HTTP": "true"},
			wantHTTP: true,
			wantAddr: "127.0.0.1:8080",
			wantPath: "/mcp",
		},
		{
			name:     "custom path",
			env:      map[string]string{"JENKINS_MCP_HTTP": "1", "JENKINS_MCP_HTTP_PATH": "/jenkins"},
			wantHTTP: true,
			wantAddr: "127.0.0.1:8080",
			wantPath: "/jenkins",
		},
		{
			name:     "flag addr overrides env addr",
			env:      map[string]string{"JENKINS_MCP_HTTP": "1.2.3.4:7000"},
			args:     []string{"--http=:9999"},
			wantHTTP: true,
			wantAddr: ":9999",
			wantPath: "/mcp",
		},
		{
			name:          "stateless via flag",
			args:          []string{"--http", "--http-stateless"},
			wantHTTP:      true,
			wantAddr:      "127.0.0.1:8080",
			wantPath:      "/mcp",
			wantStateless: true,
		},
		{
			name:          "stateless via env",
			env:           map[string]string{"JENKINS_MCP_HTTP": "1", "JENKINS_MCP_HTTP_STATELESS": "true"},
			wantHTTP:      true,
			wantAddr:      "127.0.0.1:8080",
			wantPath:      "/mcp",
			wantStateless: true,
		},
		{
			name:     "stateful is the default",
			args:     []string{"--http"},
			wantHTTP: true,
			wantAddr: "127.0.0.1:8080",
			wantPath: "/mcp",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv("JENKINS_MCP_HTTP")
			os.Unsetenv("JENKINS_MCP_HTTP_PATH")
			os.Unsetenv("JENKINS_MCP_HTTP_STATELESS")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cli, err := parseCLI(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolveTransport(cli)
			if err != nil {
				t.Fatal(err)
			}
			if got.http != tc.wantHTTP || got.addr != tc.wantAddr || got.path != tc.wantPath || got.stateless != tc.wantStateless {
				t.Errorf("resolveTransport(%v) = %+v, want http=%v addr=%q path=%q stateless=%v",
					tc.args, got, tc.wantHTTP, tc.wantAddr, tc.wantPath, tc.wantStateless)
			}
		})
	}
}

func TestCwdContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := cwdFromContext(ctx); got != "" {
		t.Fatalf("empty context: got cwd %q, want \"\"", got)
	}
	if got := cwdFromContext(withCwd(ctx, "")); got != "" {
		t.Fatalf("withCwd empty: got cwd %q, want \"\"", got)
	}
	if got := cwdFromContext(withCwd(ctx, "/repo/root")); got != "/repo/root" {
		t.Fatalf("withCwd set: got cwd %q, want /repo/root", got)
	}
}

func TestHTTPCwdFromHeaders(t *testing.T) {
	mk := func(headers map[string][]string) (string, error) {
		r := httptest.NewRequest("POST", "/mcp", nil)
		for k, vs := range headers {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
		ctx, err := httpCwdFromHeaders(context.Background(), r)
		if err != nil {
			return "", err
		}
		return cwdFromContext(ctx), nil
	}
	for name, c := range map[string]struct {
		headers map[string][]string
		want    string
	}{
		"no header":     {nil, ""},
		"X-Repo-Root":   {map[string][]string{"X-Repo-Root": {"/a"}}, "/a"},
		"X-Cwd":         {map[string][]string{"X-Cwd": {"/b"}}, "/b"},
		"both agreeing": {map[string][]string{"X-Repo-Root": {"/a"}, "X-Cwd": {"/a"}}, "/a"},
	} {
		if got, err := mk(c.headers); err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, c.want)
		}
	}
	for name, c := range map[string]struct {
		headers map[string][]string
		want    string
	}{
		"both disagreeing": {map[string][]string{"X-Repo-Root": {"/a"}, "X-Cwd": {"/b"}}, `X-Repo-Root "/a" and X-Cwd "/b" disagree`},
		"repeated header":  {map[string][]string{"X-Repo-Root": {"/a", "/b"}}, "at most one X-Repo-Root"},
		"empty header":     {map[string][]string{"X-Cwd": {""}}, "must not be empty"},
	} {
		if _, err := mk(c.headers); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

func TestCheckWorkDir(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if err := checkWorkDir(dir); err != nil {
		t.Errorf("existing directory: %v", err)
	}
	cases := map[string]string{
		f:                          "is not a directory",
		filepath.Join(dir, "nope"): "no such file or directory",
		"relative/path":            "is not an absolute path",
		"":                         "is not an absolute path",
	}
	if os.Geteuid() != 0 {
		cases[filepath.Join(locked, "inner")] = "permission denied"
	}
	for in, want := range cases {
		if err := checkWorkDir(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("checkWorkDir(%q): want error containing %q, got %v", in, want, err)
		}
	}
}

func TestGitDistinguishesAbsenceFromFailure(t *testing.T) {
	dir := t.TempDir()
	ctx := withCwd(context.Background(), dir)
	if remote, err := currentGitRemote(ctx); err != nil || remote != "" {
		t.Errorf("not a repository: got %q, %v; want no remote and no error", remote, err)
	}
	if _, err := runGit(context.Background(), "init", "-q", dir); err != nil {
		t.Fatal(err)
	}
	if remote, err := currentGitRemote(ctx); err != nil || remote != "" {
		t.Errorf("repository without origin: got %q, %v; want no remote and no error", remote, err)
	}
	if _, err := runGit(ctx, "remote", "add", "origin", "git@host:o/r.git"); err != nil {
		t.Fatal(err)
	}
	if remote, err := currentGitRemote(ctx); err != nil || remote != "git@host:o/r.git" {
		t.Errorf("origin: got %q, %v", remote, err)
	}
	top, err := runGit(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := evalSymlinks(t, top), evalSymlinks(t, dir); a != b {
		t.Errorf("git ran in %q, want %q", a, b)
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := currentGitRemote(withCwd(context.Background(), file)); err == nil || !strings.Contains(err.Error(), "git -C "+file) {
		t.Errorf("git failing for another reason must be an error naming the command, got %v", err)
	}
	t.Setenv("PATH", "")
	if _, err := currentGitRemote(ctx); err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("a missing git binary must be an error, got %v", err)
	}
}

func evalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
