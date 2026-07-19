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

	for _, tc := range []struct {
		path         string
		content      string
		cacheControl string
	}{
		{"/", "<!doctype html>", "no-cache"},
		{"/applications/example/settings", "<!doctype html>", "no-cache"},
		{"/chunk-D54LGOLI.js", "", "public, max-age=31536000, immutable"},
		{"/styles-YB2YLKR2.css", "", "public, max-age=31536000, immutable"},
	} {
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
