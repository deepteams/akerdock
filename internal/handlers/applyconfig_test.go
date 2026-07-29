package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// force_rebuild and skip_build are opposites: one rebuilds everything, the
// other builds nothing. Letting both through would silently pick one and
// deploy something the caller did not ask for.
func TestDecodeDeployBody(t *testing.T) {
	tests := map[string]struct {
		payload      string
		wantOK       bool
		wantForce    bool
		wantSkip     bool
		wantStatus   int
		wantContains string
	}{
		"empty body":          {"", true, false, false, 0, ""},
		"empty object":        {"{}", true, false, false, 0, ""},
		"force rebuild":       {`{"force_rebuild":true}`, true, true, false, 0, ""},
		"skip build":          {`{"skip_build":true}`, true, false, true, 0, ""},
		"malformed json":      {"{", true, false, false, 0, ""},
		"both flags":          {`{"force_rebuild":true,"skip_build":true}`, false, false, false, http.StatusUnprocessableEntity, "mutually exclusive"},
		"both flags reversed": {`{"skip_build":true,"force_rebuild":true}`, false, false, false, http.StatusUnprocessableEntity, "mutually exclusive"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/applications/x/deploy", strings.NewReader(tc.payload))

			body, ok := decodeDeployBody(rec, req)

			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if body.ForceRebuild != tc.wantForce || body.SkipBuild != tc.wantSkip {
				t.Fatalf("body = %+v, want force=%v skip=%v", body, tc.wantForce, tc.wantSkip)
			}
			if tc.wantOK {
				return
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tc.wantContains) {
				t.Fatalf("body = %s, want it to mention %q", rec.Body.String(), tc.wantContains)
			}
		})
	}
}
