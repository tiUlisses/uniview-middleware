package digest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildAuthorization(t *testing.T) {
	transport := NewTransport("user", "pass", http.DefaultTransport)
	transport.cnonceFn = func() (string, error) {
		return "abcdef123456", nil
	}
	challenge := digestChallenge{
		Realm:     "uniview",
		Nonce:     "nonce-value",
		Qop:       "auth",
		Algorithm: "MD5",
	}
	req, err := http.NewRequest(http.MethodPost, "http://example.com/LAPI/V1.0/System/Event/Subscription", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	auth, err := transport.buildAuthorization(req.Context(), req, challenge)
	if err != nil {
		t.Fatalf("build authorization: %v", err)
	}

	expected := "Digest username=\"user\", realm=\"uniview\", nonce=\"nonce-value\", uri=\"/LAPI/V1.0/System/Event/Subscription\", response=\"d64bc88b5eed6c3eda4553e7a9416c38\", qop=auth, nc=00000001, cnonce=\"abcdef123456\", algorithm=MD5"
	if auth != expected {
		t.Fatalf("unexpected authorization header\nexpected: %s\nactual:   %s", expected, auth)
	}
}

func TestParseChallenge(t *testing.T) {
	header := "Digest realm=\"uniview\", nonce=\"abc\", qop=\"auth\", algorithm=MD5"
	challenge, err := parseChallenge(header)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	if challenge.Realm != "uniview" || challenge.Nonce != "abc" || challenge.Qop != "auth" {
		t.Fatalf("unexpected challenge: %+v", challenge)
	}
}

func TestRoundTripWithDigest(t *testing.T) {
	challenge := "Digest realm=\"uniview\", nonce=\"abc\", qop=\"auth\", algorithm=MD5"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := NewTransport("user", "pass", nil)
	transport.cnonceFn = func() (string, error) { return "abcdef123456", nil }
	client := &http.Client{Transport: transport}

	resp, err := client.Get(server.URL + "/LAPI/V1.0/System/Event/Subscription")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
}
