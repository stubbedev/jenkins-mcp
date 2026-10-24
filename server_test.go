package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectTest wires an in-memory MCP client to the real server (built by
// newServer) so the full tools/list + tools/call path — schema passthrough,
// dispatch, arg normalization, result wrapping — is exercised end to end.
func connectTest(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := newServer("test instructions")
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestServerListsAllTools(t *testing.T) {
	cs := connectTest(t)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// Must match the embedded tools.json. A drift here means a tool was dropped
	// from registration or the embed broke.
	if got, want := len(res.Tools), len(loadToolDefs()); got != want {
		t.Fatalf("got %d tools, want %d", got, want)
	}
	for _, tool := range res.Tools {
		if tool.Name == "" {
			t.Errorf("tool with empty name")
		}
		if tool.InputSchema == nil {
			t.Errorf("%s: nil input schema (raw schema not passed through)", tool.Name)
		}
	}
}

func TestCallToolNotConfigured(t *testing.T) {
	// jenkins is the package global; unset means "not configured". Guard the
	// dispatch-time check returns an IsError result (not a protocol error).
	saved := jenkins
	jenkins = nil
	t.Cleanup(func() { jenkins = saved })

	cs := connectTest(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jenkins_get_queue",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool returned protocol error, want IsError result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true, got result: %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "not configured") {
		t.Errorf("want 'not configured' message, got %q", resultText(res))
	}
}

func TestCallToolDispatchesToJenkins(t *testing.T) {
	// Back the global client with a mock Jenkins so a real dispatch path runs
	// through the MCP layer (client → server → AddTool → dispatch → client method).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	saved := jenkins
	jenkins = NewJenkinsClient(Config{URL: srv.URL, Username: "u", Token: "t"})
	t.Cleanup(func() { jenkins = saved })

	cs := connectTest(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jenkins_list_jobs",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "No top-level jobs found") {
		t.Errorf("got %q, want empty-job-list message", resultText(res))
	}
}

func TestCallToolBuildRevision(t *testing.T) {
	sha := strings.Repeat("b", 40)
	var query string
	mux := http.NewServeMux()
	mux.HandleFunc("/git/notifyCommit", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte("Scheduled Kontainer\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	saved := jenkins
	jenkins = NewJenkinsClient(Config{URL: srv.URL, Username: "u", Token: "t", NotifyCommitToken: "nc"})
	t.Cleanup(func() { jenkins = saved })

	cs := connectTest(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jenkins_build_revision",
		Arguments: map[string]any{"url": "https://git.example.com/k.git", "branch": "feature/x", "sha": strings.ToUpper(sha)},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", resultText(res))
	}
	want := "branches=feature%2Fx&sha1=" + sha + "&token=nc&url=https%3A%2F%2Fgit.example.com%2Fk.git"
	if query != want {
		t.Errorf("query = %s, want %s", query, want)
	}
	if !strings.Contains(resultText(res), "- Kontainer") {
		t.Errorf("got %q, want the scheduled job listed", resultText(res))
	}
}

func TestBuildInstructionsMakesNoJenkinsRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"id":"alice"}`))
	}))
	t.Cleanup(srv.Close)

	cfg := Config{URL: srv.URL, Username: "alice", Token: "t"}
	saved := jenkins
	jenkins = NewJenkinsClient(cfg)
	t.Cleanup(func() { jenkins = saved })

	for _, httpMode := range []bool{true, false} {
		out := buildInstructions(&cfg, httpMode)
		if !strings.Contains(out, srv.URL+" — authenticating as alice") {
			t.Errorf("httpMode=%v: instructions missing the configured identity:\n%s", httpMode, out)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("buildInstructions made %d request(s) to Jenkins; startup must not touch the network", n)
	}
}

func TestCheckConnection(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"ok", http.StatusOK, `{"id":"alice"}`, ""},
		{"bad credentials", http.StatusUnauthorized, "nope", "Authentication failed"},
		{"no user id", http.StatusOK, `{}`, "returned no user id"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/me/api/json" {
				t.Errorf("%s: unexpected path %s", tc.name, r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		err := NewJenkinsClient(Config{URL: srv.URL, Username: "alice", Token: "t"}).checkConnection(context.Background())
		srv.Close()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "checking the Jenkins connection to "+srv.URL)):
			t.Errorf("%s: want error containing %q and the Jenkins URL, got %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestCallToolRejectsNonNumericLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Jenkins must not be queried with an invalid limit, got %s", r.URL)
	}))
	t.Cleanup(srv.Close)
	saved := jenkins
	jenkins = NewJenkinsClient(Config{URL: srv.URL, Username: "u", Token: "t"})
	t.Cleanup(func() { jenkins = saved })

	res, err := connectTest(t).CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jenkins_list_jobs",
		Arguments: map[string]any{"limit": "ten"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(res), `limit must be an integer, got "ten"`) {
		t.Errorf("want an error naming limit and the value, got %q", resultText(res))
	}
}

func TestUpdateJobConfigRejectsBadFormatBeforeWriting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Jenkins must not be contacted for an invalid format, got %s %s", r.Method, r.URL)
	}))
	t.Cleanup(srv.Close)
	saved := jenkins
	jenkins = NewJenkinsClient(Config{URL: srv.URL, Username: "u", Token: "t"})
	t.Cleanup(func() { jenkins = saved })

	res, err := connectTest(t).CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jenkins_update_job_config",
		Arguments: map[string]any{"jobPath": "demo", "format": "yaml", "patch": map[string]any{"description": "d"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(res), `format must be "toon" or "json", got "yaml"`) {
		t.Errorf("want a format error, got %q", resultText(res))
	}
}
