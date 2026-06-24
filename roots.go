package main

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// rootsTimeout bounds the roots/list round-trip so a client that declared the
// capability but never answers can't hang a tool call.
const rootsTimeout = 5 * time.Second

// rootsCache memoizes the filesystem roots a client exposed via roots/list,
// keyed by MCP session ID so concurrent sessions don't clobber each other. The
// roots/list call is a server→client round-trip; without this cache every
// git-resolving tool call would trigger one. Entries are invalidated on a
// notifications/roots/list_changed from the client.
var rootsCache sync.Map // map[string]rootsEntry

type rootsEntry struct {
	paths []string // local filesystem paths decoded from file:// root URIs
}

// rootsForSession returns every filesystem root the caller exposed via MCP
// roots, or nil when roots are unavailable or unsupported. Results are cached
// per session and refreshed on a list_changed notification. Callers decide what
// to do with multiple roots (see resolveJobPath's per-root matching).
func rootsForSession(ctx context.Context) []string {
	sess := server.ClientSessionFromContext(ctx)
	if sess == nil {
		return nil
	}
	// Only ask clients that declared the roots capability; otherwise the
	// request would block on a client that never answers.
	if info, ok := sess.(server.SessionWithClientInfo); !ok || info.GetClientCapabilities().Roots == nil {
		return nil
	}

	sid := sess.SessionID()
	if cached, ok := rootsCache.Load(sid); ok {
		return cached.(rootsEntry).paths
	}

	paths, ok := fetchRoots(ctx)
	// Cache only a definitive answer (success, including a legit empty list).
	// A transport error / timeout is left uncached so the next call retries
	// rather than poisoning the session until a list_changed that may never come.
	if ok {
		rootsCache.Store(sid, rootsEntry{paths: paths})
	}
	return paths
}

// fetchRoots performs the roots/list round-trip (bounded by rootsTimeout) and
// decodes the file:// URIs to local paths. The bool is false on a transport
// error or timeout (caller should not cache it); true on a definitive answer,
// even if the client reports zero roots.
func fetchRoots(ctx context.Context) ([]string, bool) {
	srv := server.ServerFromContext(ctx)
	if srv == nil {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, rootsTimeout)
	defer cancel()
	res, err := srv.RequestRoots(cctx, mcp.ListRootsRequest{
		Request: mcp.Request{Method: string(mcp.MethodListRoots)},
	})
	if err != nil || res == nil {
		return nil, false
	}
	var paths []string
	for _, r := range res.Roots {
		if p := fileURIToPath(r.URI); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}

// invalidateRoots drops the cached roots for the session in ctx so the next
// lookup re-fetches. Wired to notifications/roots/list_changed in main.go.
func invalidateRoots(ctx context.Context) {
	if sess := server.ClientSessionFromContext(ctx); sess != nil {
		rootsCache.Delete(sess.SessionID())
	}
}

// fileURIToPath converts a file:// URI to a local filesystem path, returning ""
// for any other scheme. A bare path (no scheme) is returned as-is.
func fileURIToPath(uri string) string {
	if uri == "" {
		return ""
	}
	if !strings.Contains(uri, "://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}
