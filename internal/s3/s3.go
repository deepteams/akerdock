// Package s3 speaks just enough of the S3 REST API to store and retrieve
// database backups: PUT, GET, HEAD, DELETE and a listing of one prefix.
//
// It is hand-written rather than pulled from the AWS SDK on purpose. The
// deliverable is a single static binary (ADR-021) and the SDK's surface —
// credential chains, IMDS probes, retry middleware — is both far larger than
// what a backup upload needs and a source of outbound calls we do not want.
// Signature V4 is a page of code; that is the whole dependency.
//
// Two ways to reach the bucket, deliberately:
//
//   - the instance signs and performs the request itself (validation,
//     retention) — the credential never leaves the process;
//   - the instance signs a *presigned URL* and the target server does the
//     transfer with curl (backup upload, restore download). A dump can be
//     gigabytes: it goes straight from the server to the bucket, never
//     through the control plane, and the credential is never on the server.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config describes one bucket and how to authenticate against it.
type Config struct {
	Endpoint   string // https://s3.eu-west-3.amazonaws.com, http://minio:9000…
	Region     string // empty defaults to us-east-1, which MinIO accepts
	Bucket     string
	PathPrefix string
	AccessKey  string
	SecretKey  string
	// SSEAlgorithm, when set (e.g. "AES256"), requests server-side encryption at
	// rest for uploads: the header is SIGNED into the presigned PUT and must be
	// sent by the uploader (see SSEHeader). Empty leaves objects unencrypted by
	// this layer — the operator may still enforce bucket default encryption.
	SSEAlgorithm string
}

// SSEHeader returns the server-side-encryption request header the uploader must
// send with a PUT, and whether one is configured. It must match the header
// signed into the presigned URL (presign), or S3 rejects the request.
func (c *Client) SSEHeader() (string, bool) {
	if c.cfg.SSEAlgorithm == "" {
		return "", false
	}
	return "x-amz-server-side-encryption: " + c.cfg.SSEAlgorithm, true
}

// Client performs signed requests against one bucket. Path-style addressing
// is used throughout: it is what self-hosted gateways (MinIO, Garage, Ceph)
// support, and AWS still accepts it.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client. The HTTP timeout is generous: a validation round trip
// is small, but a retention listing can be slow on a cold bucket.
func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) region() string {
	if c.cfg.Region == "" {
		return "us-east-1"
	}
	return c.cfg.Region
}

// Key joins the configured prefix with a relative object name.
func (c *Client) Key(name string) string {
	prefix := strings.Trim(c.cfg.PathPrefix, "/")
	name = strings.TrimPrefix(name, "/")
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// objectURL builds the path-style URL of an object key.
func (c *Client) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.cfg.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint")
	}
	u.Path = "/" + c.cfg.Bucket + "/" + key
	return u, nil
}

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Put uploads an object. Used for the small validation object only — real
// dumps are uploaded by the target server through a presigned URL.
func (c *Client) Put(ctx context.Context, key string, body []byte) error {
	u, err := c.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	c.sign(req, hex.EncodeToString(sum[:]), int64(len(body)))
	return c.do(req, http.StatusOK)
}

// Get downloads an object.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	c.sign(req, emptyPayloadHash, 0)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, statusError(res)
	}
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}

// Size returns the byte length of an object, and whether it exists.
func (c *Client) Size(ctx context.Context, key string) (int64, bool, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return 0, false, err
	}
	c.sign(req, emptyPayloadHash, 0)
	res, err := c.http.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusOK:
		return res.ContentLength, true, nil
	case http.StatusNotFound:
		return 0, false, nil
	default:
		return 0, false, statusError(res)
	}
}

// Delete removes an object. Deleting an absent object is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	u, err := c.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	c.sign(req, emptyPayloadHash, 0)
	return c.do(req, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
}

// Check performs the write/read/delete round trip that makes a storage
// usable (§20.5): a misconfigured bucket must be rejected at registration,
// not at the first backup.
func (c *Client) Check(ctx context.Context) error {
	key := c.Key(".akerdock-check")
	probe := []byte("akerdock")
	if err := c.Put(ctx, key, probe); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	got, err := c.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read failed: %w", err)
	}
	if string(got) != string(probe) {
		return fmt.Errorf("read back different content than written")
	}
	if err := c.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}
	return nil
}

// PresignPut returns a URL that uploads one object, valid for expiry. The
// URL *is* the credential for that one operation: it is handed to the target
// server through stdin, never through argv (INV-003).
func (c *Client) PresignPut(key string, expiry time.Duration) (string, error) {
	return c.presign(http.MethodPut, key, expiry)
}

// PresignGet returns a URL that downloads one object.
func (c *Client) PresignGet(key string, expiry time.Duration) (string, error) {
	return c.presign(http.MethodGet, key, expiry)
}

// --- signature v4 ------------------------------------------------------------

// sign adds the Authorization header (§ AWS SigV4, header form).
func (c *Client) sign(req *http.Request, payloadHash string, contentLength int64) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	scopeDate := now.Format("20060102")
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("Host", req.URL.Host)

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.URL.Host, payloadHash, amzDate)
	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		escapePath(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := scopeDate + "/" + c.region() + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:]),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(c.signingKey(scopeDate), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey, scope, signedHeaders, signature))
}

// presign builds a query-signed URL (§ AWS SigV4, query form): the signature
// travels in the URL, so a plain HTTP client — curl on the target server —
// can perform the operation without ever holding the secret key.
func (c *Client) presign(method, key string, expiry time.Duration) (string, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	scopeDate := now.Format("20060102")
	scope := scopeDate + "/" + c.region() + "/s3/aws4_request"

	// Server-side encryption is requested by signing the SSE header into a PUT.
	// Canonical headers must be lowercase and sorted: "host" sorts before
	// "x-amz-server-side-encryption".
	signedHeaders := "host"
	canonicalHeaders := "host:" + u.Host + "\n"
	if method == http.MethodPut && c.cfg.SSEAlgorithm != "" {
		signedHeaders = "host;x-amz-server-side-encryption"
		canonicalHeaders += "x-amz-server-side-encryption:" + c.cfg.SSEAlgorithm + "\n"
	}

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", c.cfg.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(expiry.Seconds())))
	q.Set("X-Amz-SignedHeaders", signedHeaders)

	canonicalRequest := strings.Join([]string{
		method,
		escapePath(u.Path),
		q.Encode(),
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:]),
	}, "\n")
	q.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(c.signingKey(scopeDate), []byte(stringToSign))))

	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) signingKey(scopeDate string) []byte {
	k := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), []byte(scopeDate))
	k = hmacSHA256(k, []byte(c.region()))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// escapePath percent-encodes each path segment the way S3 expects (the slash
// separators stay literal).
func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(p), "+", "%20")
	}
	return strings.Join(parts, "/")
}

// --- plumbing ----------------------------------------------------------------

func (c *Client) do(req *http.Request, accept ...int) error {
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	for _, code := range accept {
		if res.StatusCode == code {
			return nil
		}
	}
	return statusError(res)
}

// s3Error is the XML body S3 returns on failure.
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// statusError turns a failed response into an error that names the cause
// without ever echoing the request (which carries the signature).
func statusError(res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var e s3Error
	if err := xml.Unmarshal(body, &e); err == nil && e.Code != "" {
		return fmt.Errorf("s3: %s (%s)", e.Code, e.Message)
	}
	return fmt.Errorf("s3: unexpected status %d", res.StatusCode)
}
