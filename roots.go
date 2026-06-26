package main

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	ss := sessionFromContext(ctx)
	if ss == nil {
		return nil
	}

	sid := ss.ID()
	if cached, ok := rootsCache.Load(sid); ok {
		return cached.(rootsEntry).paths
	}

	paths, ok := fetchRoots(ctx, ss)
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
// error or timeout, or a client that does not support roots (caller should not
// cache it); true on a definitive answer, even if the client reports zero roots.
//
// ponytail: no capability pre-check — the SDK returns method-not-found fast for
// clients without roots, and rootsTimeout bounds the slow case. Add a gate only
// if a non-conforming client (declares nothing, never replies) shows up.
func fetchRoots(ctx context.Context, ss *mcp.ServerSession) ([]string, bool) {
	cctx, cancel := context.WithTimeout(ctx, rootsTimeout)
	defer cancel()
	res, err := ss.ListRoots(cctx, &mcp.ListRootsParams{})
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

// invalidateRoots drops the cached roots for the session so the next lookup
// re-fetches. Wired to notifications/roots/list_changed in main.go.
func invalidateRoots(ss *mcp.ServerSession) {
	if ss != nil {
		rootsCache.Delete(ss.ID())
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
