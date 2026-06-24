package main

import (
	"context"
	"net/http/httptest"
	"os"
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
			name:     "flag does not eat following flag",
			args:     []string{"--http", "--version"},
			wantHTTP: true,
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
			got := resolveTransport(tc.args)
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
	mk := func(headers map[string]string) string {
		r := httptest.NewRequest("POST", "/mcp", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return cwdFromContext(httpCwdFromHeaders(context.Background(), r))
	}
	if got := mk(nil); got != "" {
		t.Errorf("no header: got %q, want \"\"", got)
	}
	if got := mk(map[string]string{"X-Repo-Root": "/a"}); got != "/a" {
		t.Errorf("X-Repo-Root: got %q, want /a", got)
	}
	if got := mk(map[string]string{"X-Cwd": "/b"}); got != "/b" {
		t.Errorf("X-Cwd: got %q, want /b", got)
	}
	// X-Repo-Root wins over X-Cwd.
	if got := mk(map[string]string{"X-Repo-Root": "/a", "X-Cwd": "/b"}); got != "/a" {
		t.Errorf("both headers: got %q, want /a", got)
	}
}

func TestIsRepoDir(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/file"
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{dir, true},            // existing absolute dir
		{f, false},             // existing file, not a dir
		{dir + "/nope", false}, // missing
		{"relative/path", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isRepoDir(c.in); got != c.want {
			t.Errorf("isRepoDir(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSafeGitUsesContextCwd(t *testing.T) {
	dir := t.TempDir()
	if out := safeGit(context.Background(), "init", dir); out == "" {
		// `git init <dir>` prints to stdout on success; empty means git missing.
		t.Skip("git not available or produced no output")
	}
	// rev-parse --show-toplevel from within dir (via -C) should resolve to dir.
	got := safeGit(withCwd(context.Background(), dir), "rev-parse", "--show-toplevel")
	if got == "" {
		t.Skip("git rev-parse produced no output")
	}
	// macOS /tmp symlinks to /private/tmp, so compare suffixes loosely.
	if resolved, err := os.Stat(got); err != nil || resolved == nil {
		t.Errorf("toplevel %q does not exist: %v", got, err)
	}
}
