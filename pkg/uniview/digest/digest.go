package digest

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	algorithmMD5 = "MD5"
)

type cnonceFunc func() (string, error)

// Transport implements HTTP Digest Auth (RFC 2617) for Uniview LiteAPI cameras.
// It performs a 401 challenge/response handshake and retries the request.
//
// NOTE: Field names and behavior should be validated against the LiteAPI PDF.
// This transport focuses strictly on RFC 2617 digest handling.
//
// It supports qop="auth" and algorithm=MD5.
// Other algorithms/qop values return an error.
//
// The transport is safe for concurrent use.
type Transport struct {
	Username string
	Password string

	Base http.RoundTripper

	mu         sync.Mutex
	nonceCount uint64
	cnonceFn   cnonceFunc
}

// NewTransport creates a digest transport with default settings.
func NewTransport(username, password string, base http.RoundTripper) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{
		Username: username,
		Password: password,
		Base:     base,
		cnonceFn: defaultCnonce,
	}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("digest transport: nil request")
	}
	bodyBytes, err := drainBody(req)
	if err != nil {
		return nil, fmt.Errorf("digest transport: read request body: %w", err)
	}

	clone := cloneRequest(req, bodyBytes)
	resp, err := t.Base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if challenge == "" {
		return resp, nil
	}
	if err := resp.Body.Close(); err != nil {
		return nil, fmt.Errorf("digest transport: close 401 body: %w", err)
	}

	digestFields, err := parseChallenge(challenge)
	if err != nil {
		return nil, fmt.Errorf("digest transport: parse challenge: %w", err)
	}
	authorization, err := t.buildAuthorization(req.Context(), req, digestFields)
	if err != nil {
		return nil, fmt.Errorf("digest transport: build authorization: %w", err)
	}

	retry := cloneRequest(req, bodyBytes)
	retry.Header.Set("Authorization", authorization)
	return t.Base.RoundTrip(retry)
}

func (t *Transport) buildAuthorization(ctx context.Context, req *http.Request, challenge digestChallenge) (string, error) {
	if strings.EqualFold(challenge.Algorithm, "") {
		challenge.Algorithm = algorithmMD5
	}
	if !strings.EqualFold(challenge.Algorithm, algorithmMD5) {
		return "", fmt.Errorf("unsupported digest algorithm: %s", challenge.Algorithm)
	}

	qop := pickQop(challenge.Qop)
	if qop != "" && qop != "auth" {
		return "", fmt.Errorf("unsupported qop: %s", qop)
	}

	cnonce, err := t.cnonceFn()
	if err != nil {
		return "", fmt.Errorf("generate cnonce: %w", err)
	}

	nc := t.nextNonceCount()
	uri := req.URL.RequestURI()

	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", t.Username, challenge.Realm, t.Password))
	ha2 := md5Hex(fmt.Sprintf("%s:%s", req.Method, uri))

	var response string
	if qop != "" {
		response = md5Hex(fmt.Sprintf("%s:%s:%s:%s:%s:%s", ha1, challenge.Nonce, nc, cnonce, qop, ha2))
	} else {
		response = md5Hex(fmt.Sprintf("%s:%s:%s", ha1, challenge.Nonce, ha2))
	}

	parts := []string{
		"Digest username=\"" + t.Username + "\"",
		"realm=\"" + challenge.Realm + "\"",
		"nonce=\"" + challenge.Nonce + "\"",
		"uri=\"" + uri + "\"",
		"response=\"" + response + "\"",
	}
	if challenge.Opaque != "" {
		parts = append(parts, "opaque=\""+challenge.Opaque+"\"")
	}
	if qop != "" {
		parts = append(parts, "qop="+qop, "nc="+nc, "cnonce=\""+cnonce+"\"")
	}
	if challenge.Algorithm != "" {
		parts = append(parts, "algorithm="+challenge.Algorithm)
	}
	return strings.Join(parts, ", "), nil
}

func (t *Transport) nextNonceCount() string {
	count := atomic.AddUint64(&t.nonceCount, 1)
	return fmt.Sprintf("%08x", count)
}

func defaultCnonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func md5Hex(value string) string {
	h := md5.Sum([]byte(value))
	return hex.EncodeToString(h[:])
}

type digestChallenge struct {
	Realm     string
	Nonce     string
	Qop       string
	Algorithm string
	Opaque    string
}

func parseChallenge(header string) (digestChallenge, error) {
	if !strings.HasPrefix(strings.ToLower(header), "digest ") {
		return digestChallenge{}, fmt.Errorf("unsupported auth scheme: %s", header)
	}
	trimmed := strings.TrimSpace(header[len("Digest "):])
	pairs := splitHeader(trimmed)
	challenge := digestChallenge{}
	for _, pair := range pairs {
		key, val, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		val = strings.TrimSpace(strings.Trim(val, "\""))
		switch key {
		case "realm":
			challenge.Realm = val
		case "nonce":
			challenge.Nonce = val
		case "qop":
			challenge.Qop = val
		case "algorithm":
			challenge.Algorithm = val
		case "opaque":
			challenge.Opaque = val
		}
	}
	if challenge.Realm == "" || challenge.Nonce == "" {
		return digestChallenge{}, errors.New("missing digest realm/nonce")
	}
	return challenge, nil
}

func pickQop(value string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ",")
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "auth" {
			return "auth"
		}
	}
	return strings.TrimSpace(parts[0])
}

func splitHeader(value string) []string {
	var parts []string
	var buf strings.Builder
	inQuotes := false
	for _, r := range value {
		switch r {
		case '"':
			inQuotes = !inQuotes
			buf.WriteRune(r)
		case ',':
			if inQuotes {
				buf.WriteRune(r)
			} else {
				parts = append(parts, strings.TrimSpace(buf.String()))
				buf.Reset()
			}
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		parts = append(parts, strings.TrimSpace(buf.String()))
	}
	return parts
}

func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer body.Close()
		return io.ReadAll(body)
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if err := req.Body.Close(); err != nil {
		return nil, err
	}
	return data, nil
}

func cloneRequest(req *http.Request, body []byte) *http.Request {
	clone := req.Clone(req.Context())
	if body != nil {
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
		clone.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	return clone
}

// WithTimeout returns a context with a default timeout if none is set.
func WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}
