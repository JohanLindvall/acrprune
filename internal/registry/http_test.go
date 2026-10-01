package registry

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHTTPClientPreservesCallerPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	base := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client := NewHTTPClient(base)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the caller's unfollowed redirect", resp.StatusCode)
	}
	if client.Timeout != base.Timeout || base.Transport != nil {
		t.Error("client options were lost or the original client was mutated")
	}
	if NewHTTPClient(nil).Timeout != 2*time.Minute {
		t.Error("default requests need a finite timeout including the body")
	}
}

func TestHTTPClientTimeoutIncludesResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := NewHTTPClient(&http.Client{Timeout: 50 * time.Millisecond})
	resp, err := client.Get(server.URL)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil || !Transient(err) {
		t.Errorf("stalled response body = %v, want a retryable timeout", err)
	}
}

func TestRedactHTTPError(t *testing.T) {
	for _, raw := range []string{"https://user:password@storage.test/blob?signature=secret#fragment", "%"} {
		cause := io.ErrUnexpectedEOF
		err := RedactError(&url.Error{Op: "Get", URL: raw, Err: cause})
		if !errors.Is(err, cause) {
			t.Error("redaction lost the cause")
		}
		for _, secret := range []string{"password", "signature", "secret", "fragment"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("redacted error leaks %s: %v", secret, err)
			}
		}
	}
}
