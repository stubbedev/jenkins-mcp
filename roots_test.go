package main

import "testing"

func TestFileURIToPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"file:///home/me/repo", "/home/me/repo"},
		{"file:///tmp/with%20space", "/tmp/with space"},
		{"/already/a/path", "/already/a/path"},
		{"", ""},
		{"https://example.com/x", ""},
		{"vscode://file/x", ""},
	}
	for _, c := range cases {
		if got := fileURIToPath(c.in); got != c.want {
			t.Errorf("fileURIToPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
