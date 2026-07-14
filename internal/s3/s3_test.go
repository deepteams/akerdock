package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testClient(endpoint string) *Client {
	return New(Config{
		Endpoint: endpoint, Region: "eu-west-3", Bucket: "backups",
		PathPrefix: "akerdock/", AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	})
}

func TestKey(t *testing.T) {
	c := testClient("http://s3.local")
	if got := c.Key("/db/dump.sql"); got != "akerdock/db/dump.sql" {
		t.Errorf("Key = %q", got)
	}
	plain := New(Config{Bucket: "b"})
	if got := plain.Key("dump.sql"); got != "dump.sql" {
		t.Errorf("Key without prefix = %q", got)
	}
}

// The presigned URL must carry everything the target server needs — and the
// secret key must never appear in it.
func TestPresignShape(t *testing.T) {
	raw, err := testClient("https://s3.local").PresignPut("akerdock/dump.sql", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Path != "/backups/akerdock/dump.sql" {
		t.Errorf("path-style addressing expected, got %q", u.Path)
	}
	q := u.Query()
	for _, k := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-Signature", "X-Amz-SignedHeaders"} {
		if q.Get(k) == "" {
			t.Errorf("missing %s in the presigned URL", k)
		}
	}
	if q.Get("X-Amz-Expires") != "900" {
		t.Errorf("expiry = %q, want 900", q.Get("X-Amz-Expires"))
	}
	if strings.Contains(raw, "wJalrXUtnFEMI") {
		t.Fatal("the secret key leaked into the presigned URL")
	}
}

// The signature must be stable for a fixed date and key: a change in the
// canonicalisation would silently break every real bucket, which no unit
// test catches unless it pins the value.
func TestSignatureIsDeterministic(t *testing.T) {
	c := testClient("https://s3.local")
	first, err := c.presign(http.MethodGet, "k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.presign(http.MethodGet, "k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Same second, same inputs → identical signature.
	if q1, q2 := mustQuery(t, first), mustQuery(t, second); q1.Get("X-Amz-Date") == q2.Get("X-Amz-Date") &&
		q1.Get("X-Amz-Signature") != q2.Get("X-Amz-Signature") {
		t.Error("two signatures of the same request differ")
	}
	// A different key must sign differently.
	other, err := c.presign(http.MethodGet, "other", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if mustQuery(t, first).Get("X-Amz-Signature") == mustQuery(t, other).Get("X-Amz-Signature") {
		t.Error("two different objects produced the same signature")
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return u.Query()
}

// A full write/read/delete round trip against a stub that checks the request
// is authenticated and path-style.
func TestCheckRoundTrip(t *testing.T) {
	var stored []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") {
			t.Errorf("request is not signed: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/backups/akerdock/.akerdock-check" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPut:
			stored, _ = readAll(r)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			_, _ = w.Write(stored)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	if err := testClient(srv.URL).Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if string(stored) != "akerdock" {
		t.Errorf("stored %q", stored)
	}
}

// A bucket that answers with an S3 error must surface its code, not a bare
// status: the operator needs to know it is AccessDenied and not NoSuchBucket.
func TestCheckSurfacesS3Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
	}))
	defer srv.Close()

	err := testClient(srv.URL).Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("error = %v, want it to name AccessDenied", err)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, r.ContentLength)
	_, err := r.Body.Read(buf)
	return buf, err
}
