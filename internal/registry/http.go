package registry

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Keep enough idle connections for the default parallelism on HTTP/1.1 too;
// net/http's default of two otherwise repeatedly reconnects between batches.
var httpTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 32
	return t
}()

// NewHTTPClient copies base and adds the redirect policy used by both registry
// backends. A nil base uses a two-minute timeout, including reading the body.
// Credentials never follow a read to another origin; mutations must stay on
// their origin and retain their method (Go otherwise turns a redirected DELETE
// into a GET). The caller's redirect policy can further restrict redirects.
func NewHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{Timeout: 2 * time.Minute}
	}
	client := *base
	if client.Transport == nil {
		client.Transport = httpTransport
	}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if err := checkRedirect(next, via); err != nil {
			return &redirectError{err}
		}
		if base.CheckRedirect != nil {
			if err := base.CheckRedirect(next, via); err != nil {
				// ErrUseLastResponse is special to net/http: do not wrap it.
				if err == http.ErrUseLastResponse {
					return err
				}
				return &redirectError{err}
			}
		}
		if !SameOrigin(next.URL, via[0].URL) {
			next.Header.Del("Authorization")
			next.Header.Del("Proxy-Authorization")
			next.Header.Del("Cookie")
		}
		return nil
	}
	return &client
}

func checkRedirect(next *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if next.URL.User != nil || (next.URL.Scheme != "https" && next.URL.Scheme != "http") ||
		(via[0].URL.Scheme == "https" && next.URL.Scheme != "https") {
		return errors.New("refusing an unsafe redirect")
	}
	if next.Method != via[0].Method {
		return fmt.Errorf("refusing a redirect that turns %s into %s", via[0].Method, next.Method)
	}
	if !SameOrigin(next.URL, via[0].URL) && via[0].Method != http.MethodGet && via[0].Method != http.MethodHead {
		return errors.New("refusing to redirect a mutation to another origin")
	}
	return nil
}

// SameOrigin compares URL schemes, hosts and ports, without treating a
// subdomain or another port as a safe destination for credentials.
func SameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

type redirectError struct{ err error }

func (e *redirectError) Error() string { return e.err.Error() }
func (e *redirectError) Unwrap() error { return e.err }
func (e *redirectError) NonRetriable() {}

// Transient reports network failures worth retrying. Configuration failures
// (DNS names that do not exist, certificate errors and refused redirects)
// fail immediately. A canceled caller is checked separately by each backend.
func Transient(err error) bool {
	var nonRetriable interface{ NonRetriable() }
	if errors.As(err, &nonRetriable) {
		return false
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	if untrustedCertificate(err) {
		return false
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return true
	}
	if opErr, ok := errors.AsType[*net.OpError](err); ok {
		switch opErr.Op {
		case "dial", "read", "write", "proxyconnect":
			return true
		}
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.Contains(err.Error(), "transport connection broken")
}

func untrustedCertificate(err error) bool {
	_, verification := errors.AsType[*tls.CertificateVerificationError](err)
	_, authority := errors.AsType[x509.UnknownAuthorityError](err)
	_, hostname := errors.AsType[x509.HostnameError](err)
	_, invalid := errors.AsType[x509.CertificateInvalidError](err)
	return verification || authority || hostname || invalid
}

// RedactError removes credentials and query signatures from a failed HTTP
// request's URL while retaining its error chain for retry classification.
func RedactError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	clean := *urlErr
	u, parseErr := url.Parse(urlErr.URL)
	if parseErr != nil {
		clean.URL = "(unparsable URL)"
	} else {
		clean.URL = RedactURL(u)
	}
	return &clean
}

// BufferResponse reads a successful document while still inside the backend's
// retry loop. A server can send success headers and then truncate or stall its
// body; handing that stream to the caller would bypass retries. The network
// body is always closed and memory use is bounded by MaxDocumentSize.
func BufferResponse(resp *http.Response) error {
	data, err := ReadDocument(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return err
}
