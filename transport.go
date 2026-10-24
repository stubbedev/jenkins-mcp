package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

const (
	defaultHTTPAddr = "127.0.0.1:8080"
	defaultHTTPPath = "/mcp"
)

func resolveTransport(cli cliOptions) (transportConfig, error) {
	tc := transportConfig{addr: defaultHTTPAddr, path: defaultHTTPPath}
	if p, set := os.LookupEnv("JENKINS_MCP_HTTP_PATH"); set {
		if !strings.HasPrefix(p, "/") {
			return transportConfig{}, fmt.Errorf("JENKINS_MCP_HTTP_PATH must start with \"/\", got %q", p)
		}
		tc.path = p
	}
	if env, set := os.LookupEnv("JENKINS_MCP_HTTP"); set && env != "" {
		switch strings.ToLower(env) {
		case "1", "true":
			tc.http = true
		case "0", "false":
		default:
			tc.http = true
			tc.addr = env
		}
	}
	if env, set := os.LookupEnv("JENKINS_MCP_HTTP_STATELESS"); set && env != "" {
		switch strings.ToLower(env) {
		case "1", "true":
			tc.stateless = true
		case "0", "false":
		default:
			return transportConfig{}, fmt.Errorf("JENKINS_MCP_HTTP_STATELESS must be true, false, 1 or 0, got %q", env)
		}
	}
	if cli.http {
		tc.http = true
		if cli.httpAddr != "" {
			tc.addr = cli.httpAddr
		}
	}
	if cli.httpStateless {
		tc.stateless = true
	}
	if tc.stateless && !tc.http {
		return transportConfig{}, errors.New("--http-stateless / JENKINS_MCP_HTTP_STATELESS needs HTTP mode (--http or JENKINS_MCP_HTTP)")
	}
	if tc.http {
		if err := validateListenAddr(tc.addr); err != nil {
			return transportConfig{}, err
		}
	}
	return tc, nil
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid HTTP listen address %q: %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid HTTP listen address %q: port must be a number from 0 to 65535", addr)
	}
	return nil
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
func serveHTTP(s *mcp.Server, tc transportConfig) error {
	opts := &mcp.StreamableHTTPOptions{Stateless: tc.stateless}
	if !tc.stateless {
		opts.SessionTimeout = sessionIdleTTL
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, opts)

	// Own the mux so a liveness endpoint can sit alongside the MCP endpoint —
	// proxies and orchestrators probe /healthz to decide if the backend is up.
	mux := http.NewServeMux()
	httpServer := &http.Server{Addr: tc.addr, Handler: mux}

	mux.Handle(tc.path, withRepoRootHeader(handler))
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
		_ = httpServer.Shutdown(ctx)
	}()

	mode := "stateful"
	if tc.stateless {
		mode = "stateless"
	}
	logErr("[jenkins-mcp] HTTP listening on " + tc.addr + tc.path + " (" + mode + ")")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// withRepoRootHeader lifts the caller-supplied repo root out of the request
// headers into the request context so the SDK passes it through to tool handlers
// and git commands run in the right working tree.
func withRepoRootHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := httpCwdFromHeaders(r.Context(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// httpCwdFromHeaders lifts the caller-supplied repo root out of the request
// headers into the context so git commands run in the right working tree.
func httpCwdFromHeaders(ctx context.Context, r *http.Request) (context.Context, error) {
	root, cwd := r.Header.Values("X-Repo-Root"), r.Header.Values("X-Cwd")
	if len(root) > 1 || len(cwd) > 1 {
		return nil, errors.New("send at most one X-Repo-Root and one X-Cwd header")
	}
	dirs := slices.Compact(slices.Sorted(slices.Values(append(root, cwd...))))
	switch len(dirs) {
	case 0:
		return ctx, nil
	case 1:
		if dirs[0] == "" {
			return nil, errors.New("X-Repo-Root / X-Cwd must not be empty")
		}
		return withCwd(ctx, dirs[0]), nil
	}
	return nil, fmt.Errorf("X-Repo-Root %q and X-Cwd %q disagree; send one of them", root[0], cwd[0])
}
