package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	jenkins = NewJenkinsClient(srv.URL, "u", "t", nil)
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
