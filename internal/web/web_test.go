package web

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerServesIndexAssetsAndSPARoutes(t *testing.T) {
	handler := Handler()
	if handler == nil {
		t.Fatal("the embedded dashboard build is missing")
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	// Hashed asset names change on EVERY rebuild of the dashboard: the test
	// must discover them in the embedded build, or it breaks at the first
	// redesign for reasons that have nothing to do with the handler.
	var hashedAssets []string
	entries, err := fs.ReadDir(dist, "dist")
	if err != nil {
		t.Fatalf("reading the embedded build: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "chunk-") && strings.HasSuffix(name, ".js") {
			hashedAssets = append(hashedAssets, "/"+name)
			break
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "styles-") && strings.HasSuffix(name, ".css") {
			hashedAssets = append(hashedAssets, "/"+name)
			break
		}
	}
	if len(hashedAssets) != 2 {
		t.Fatalf("expected a chunk-*.js and a styles-*.css in the embedded build, found %v", hashedAssets)
	}

	cases := []struct {
		path         string
		content      string
		cacheControl string
	}{
		{"/", "<!doctype html>", "no-cache"},
		{"/applications/example/settings", "<!doctype html>", "no-cache"},
		{hashedAssets[0], "", "public, max-age=31536000, immutable"},
		{hashedAssets[1], "", "public, max-age=31536000, immutable"},
	}
	for _, tc := range cases {
		response, err := http.Get(server.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", tc.path, readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d", tc.path, response.StatusCode)
		}
		if tc.content != "" && !strings.Contains(strings.ToLower(string(body)), tc.content) {
			t.Errorf("%s did not serve index.html", tc.path)
		}
		if got := response.Header.Get("Cache-Control"); got != tc.cacheControl {
			t.Errorf("%s Cache-Control = %q, want %q", tc.path, got, tc.cacheControl)
		}
	}
}

func TestHandlerReturnsNilWithoutDashboard(t *testing.T) {
	if got := handler(fstest.MapFS{}); got != nil {
		t.Fatalf("handler without index = %T, want nil", got)
	}
	if got := handler(failingSubFS{}); got != nil {
		t.Fatalf("handler with unreadable filesystem = %T, want nil", got)
	}
}

type failingSubFS struct{}

func (failingSubFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
func (failingSubFS) Sub(string) (fs.FS, error)    { return nil, fs.ErrPermission }
