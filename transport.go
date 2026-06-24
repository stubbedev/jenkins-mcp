package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// sessionIdleTTL reaps transport state for sessions a client abandoned without
// a clean DELETE, so a long-lived HTTP server doesn't accumulate dead sessions.
const sessionIdleTTL = 30 * time.Minute

// transportConfig describes how the server should listen for MCP traffic.
type transportConfig struct {
	// http is true when serving Streamable HTTP instead of stdio.
	http bool
	// addr is the listen address for HTTP mode, e.g. "127.0.0.1:8080".
	addr string
	// path is the HTTP endpoint path, default "/mcp".
	path string
	// stateless drops persistent sessions. It load-balances cleanly behind a
	// cloud proxy, but disables server→client requests (the roots/list repo
	// lookup and the multi-job elicitation picker). Default is stateful, which
	// is the right choice for a local proxy.
	stateless bool
}

// resolveTransport decides between stdio (default) and Streamable HTTP based on
// CLI flags and environment, matching this precedence:
//
//	--http[=addr] / --http addr  → HTTP on addr (or the env/default addr)
//	JENKINS_MCP_HTTP=addr        → HTTP on addr ("1"/"true" means default addr)
//	(otherwise)                  → stdio
//
// The endpoint path comes from JENKINS_MCP_HTTP_PATH (default "/mcp").
func resolveTransport(args []string) transportConfig {
	tc := transportConfig{
		addr: "127.0.0.1:8080",
		path: "/mcp",
	}
	if p := os.Getenv("JENKINS_MCP_HTTP_PATH"); p != "" {
		tc.path = p
	}

	if env := os.Getenv("JENKINS_MCP_HTTP"); env != "" {
		tc.http = true
		if env != "1" && strings.ToLower(env) != "true" {
			tc.addr = env
		}
	}
	if env := strings.ToLower(os.Getenv("JENKINS_MCP_HTTP_STATELESS")); env == "1" || env == "true" {
		tc.stateless = true
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--http":
			tc.http = true
			// Consume a following bare addr (not another flag), if present.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				tc.addr = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--http="):
			tc.http = true
			tc.addr = strings.TrimPrefix(a, "--http=")
		case a == "--http-stateless":
			tc.stateless = true
		}
	}
	return tc
}

// serveHTTP runs the MCP server over Streamable HTTP. It is stateful by default
// (persistent sessions), which keeps server→client requests working: the
// roots/list repo-root lookup and the multi-job elicitation picker. A local
// proxy is single-tenant, so there is no load-balancer downside; pass
// --http-stateless only when fronting it with a cloud LB.
//
// The cwd/repoRoot a caller's git commands should run in is read per request
// from the X-Repo-Root (or X-Cwd) header and threaded into the request context
// for git.go. When no header (or cwd arg) is supplied, resolution falls back to
// the client's MCP roots (see roots.go).
func serveHTTP(s *server.MCPServer, tc transportConfig) error {
	// Own the mux so a liveness endpoint can sit alongside the MCP endpoint —
	// proxies and orchestrators probe /healthz to decide if the backend is up.
	mux := http.NewServeMux()
	httpServer := &http.Server{Addr: tc.addr, Handler: mux}

	opts := []server.StreamableHTTPOption{
		server.WithStateLess(tc.stateless),
		server.WithEndpointPath(tc.path),
		server.WithHTTPContextFunc(httpCwdFromHeaders),
		server.WithStreamableHTTPServer(httpServer),
	}
	if !tc.stateless {
		opts = append(opts, server.WithSessionIdleTTL(sessionIdleTTL))
	}
	httpSrv := server.NewStreamableHTTPServer(s, opts...)

	mux.Handle(tc.path, httpSrv)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok jenkins-mcp " + Version + "\n"))
	})

	// Drain in-flight requests on a termination signal instead of dropping them.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		logErr("[jenkins-mcp] shutting down HTTP server")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	mode := "stateful"
	if tc.stateless {
		mode = "stateless"
	}
	logErr("[jenkins-mcp] HTTP listening on " + tc.addr + tc.path + " (" + mode + ")")
	if err := httpSrv.Start(tc.addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// httpCwdFromHeaders lifts the caller-supplied repo root out of the request
// headers into the context so git commands run in the right working tree.
func httpCwdFromHeaders(ctx context.Context, r *http.Request) context.Context {
	dir := r.Header.Get("X-Repo-Root")
	if dir == "" {
		dir = r.Header.Get("X-Cwd")
	}
	return withCwd(ctx, dir)
}
