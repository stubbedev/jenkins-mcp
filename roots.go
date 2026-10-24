package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

func rootsForSession(ctx context.Context) ([]string, error) {
	ss := sessionFromContext(ctx)
	if ss == nil || !rootsAskable(ss) || !clientDeclaresRoots(ss.InitializeParams()) {
		return nil, nil
	}
	sid := ss.ID()
	if cached, ok := rootsCache.Load(sid); ok {
		return cached.(rootsEntry).paths, nil
	}
	cctx, cancel := context.WithTimeout(ctx, rootsTimeout)
	defer cancel()
	res, err := ss.ListRoots(cctx, &mcp.ListRootsParams{})
	if err != nil {
		return nil, fmt.Errorf("asking the client for its workspace roots (roots/list): %w", err)
	}
	paths := make([]string, 0, len(res.Roots))
	for _, r := range res.Roots {
		p, local, err := fileURIToPath(r.URI)
		if err != nil {
			return nil, fmt.Errorf("workspace root %q from roots/list: %w", r.URI, err)
		}
		if local {
			paths = append(paths, p)
		}
	}
	rootsCache.Store(sid, rootsEntry{paths: paths})
	return paths, nil
}

func clientDeclaresRoots(ip *mcp.InitializeParams) bool {
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.RootsV2 != nil
}

// invalidateRoots drops the cached roots for the session so the next lookup
// re-fetches. Wired to notifications/roots/list_changed in main.go.
func invalidateRoots(ss *mcp.ServerSession) {
	if ss != nil {
		rootsCache.Delete(ss.ID())
	}
}

func fileURIToPath(uri string) (string, bool, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", false, err
	}
	switch {
	case u.Scheme == "":
		return "", false, errors.New("roots must be URIs; a bare path is not one")
	case u.Scheme != "file":
		return "", false, nil
	case u.Host != "" && u.Host != "localhost":
		return "", false, fmt.Errorf("file URI on host %q is not a local path", u.Host)
	case u.Path == "":
		return "", false, errors.New("file URI has no path")
	}
	return u.Path, true, nil
}

// rootsRemovedFrom is the first protocol revision that forbids server-initiated
// JSON-RPC requests (SEP-2322 / SEP-2575): from there on a server cannot ask a
// client for its roots at all. ISO dates compare correctly as strings.
const rootsRemovedFrom = "2026-07-28"

// rootsAskable reports whether roots/list may still be sent on this session.
func rootsAskable(ss *mcp.ServerSession) bool {
	if ss == nil {
		return false
	}
	return protocolAllowsRoots(ss.InitializeParams())
}

// protocolAllowsRoots is rootsAskable's decision, split out so it can be tested
// without a live session. A nil InitializeParams means the session has not
// finished initializing; asking then is pointless either way.
func protocolAllowsRoots(ip *mcp.InitializeParams) bool {
	return ip != nil && ip.ProtocolVersion < rootsRemovedFrom
}
