package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bodyLimit must cap the request body: a payload over the limit fails to read
// (§23.3), so a handler can never pull an unbounded body into memory.
func TestBodyLimitRejectsOversizedBody(t *testing.T) {
	var readErr error
	h := bodyLimit(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))

	// Just over the cap.
	big := strings.NewReader(strings.Repeat("a", maxRequestBody+1))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/anything", big)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if readErr == nil {
		t.Fatal("reading an oversized body succeeded — the cap is not enforced")
	}

	// A small body passes through untouched.
	readErr = nil
	small := strings.NewReader("{}")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/anything", small)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if readErr != nil {
		t.Fatalf("a small body was rejected: %v", readErr)
	}
}
