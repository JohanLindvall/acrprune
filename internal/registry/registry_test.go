package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseAddress(t *testing.T) {
	t.Setenv(ACRSuffixesEnv, "")
	tests := []struct {
		in       string
		want     Address
		location string
	}{
		{"myreg", Address{Kind: ACR, Host: "myreg.azurecr.io"}, "myreg.azurecr.io"},
		{"myreg.azurecr.io", Address{Kind: ACR, Host: "myreg.azurecr.io"}, "myreg.azurecr.io"},
		{"myreg.azurecr.cn", Address{Kind: ACR, Host: "myreg.azurecr.cn"}, "myreg.azurecr.cn"},
		{"myreg.azurecr.us/", Address{Kind: ACR, Host: "myreg.azurecr.us"}, "myreg.azurecr.us"},
		{"https://myreg.azurecr.io", Address{Kind: ACR, Host: "myreg.azurecr.io"}, "myreg.azurecr.io"},
		{"HTTPS://MyReg.AzureCR.io/", Address{Kind: ACR, Host: "myreg.azurecr.io"}, "myreg.azurecr.io"},
		// Dedicated data endpoint (DNL-scoped) login servers carry a hash.
		{"myreg-abc123.azurecr.io", Address{Kind: ACR, Host: "myreg-abc123.azurecr.io"}, "myreg-abc123.azurecr.io"},
		{"ghcr.io/myorg", Address{Kind: GHCR, Host: "ghcr.io", Owner: "myorg"}, "ghcr.io/myorg"},
		{"https://ghcr.io/myorg", Address{Kind: GHCR, Host: "ghcr.io", Owner: "myorg"}, "ghcr.io/myorg"},
		// Image references spell the owner in lowercase.
		{"GHCR.io/MyOrg/", Address{Kind: GHCR, Host: "ghcr.io", Owner: "myorg"}, "ghcr.io/myorg"},
	}
	for _, tt := range tests {
		got, err := ParseAddress(tt.in)
		if err != nil {
			t.Errorf("ParseAddress(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseAddress(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
		if got.String() != tt.location {
			t.Errorf("ParseAddress(%q).String() = %q, want %q", tt.in, got.String(), tt.location)
		}
	}

	for _, bad := range []string{
		"",
		"ghcr.io",         // GHCR needs the owner
		"ghcr.io/",        // likewise
		"ghcr.io/org/app", // the owner alone, not a repository
		"quay.io/org",     // not a supported registry
		"..",              // would name a directory outside the cache
		"my reg",
		"https://",
	} {
		if got, err := ParseAddress(bad); err == nil {
			t.Errorf("ParseAddress(%q) = %+v, want an error", bad, got)
		}
	}

	// Hosts that are not ACR login servers would receive the Azure
	// credential's registry token, so they are refused, naming what is
	// accepted.
	for _, bad := range []string{
		"quay.io",
		"docker.io",
		"myreg.azurecr.co",             // a typo
		"myregazurecr.io",              // a look-alike
		"evil.example.com",             // anything else
		"myreg.azurecr.io.example.com", // a suffix is a suffix
		"myreg.westeurope.azurecr.io",  // one label before the suffix
		"http://myreg.azurecr.io",      // never in the clear
		"myreg.azurecr.de",             // unknown until listed
	} {
		got, err := ParseAddress(bad)
		if err == nil {
			t.Errorf("ParseAddress(%q) = %+v, want an error", bad, got)
			continue
		}
		for _, want := range []string{"unsupported registry", "myreg.azurecr.io, myreg.azurecr.cn, myreg.azurecr.us", ACRSuffixesEnv, "ghcr.io/<owner>"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseAddress(%q) error = %v, want it to mention %q", bad, err, want)
			}
		}
	}
}

// TestParseAddressExtraSuffixes: other clouds' suffixes are accepted once
// listed explicitly.
func TestParseAddressExtraSuffixes(t *testing.T) {
	t.Setenv(ACRSuffixesEnv, " azurecr.de, .Private.Example ,,")
	for in, want := range map[string]string{
		"myreg.azurecr.de":         "myreg.azurecr.de",
		"myreg.private.example":    "myreg.private.example",
		"myreg.azurecr.io":         "myreg.azurecr.io",
		"https://myreg.azurecr.de": "myreg.azurecr.de",
	} {
		got, err := ParseAddress(in)
		if err != nil || got != (Address{Kind: ACR, Host: want}) {
			t.Errorf("ParseAddress(%q) = %+v, %v; want ACR at %s", in, got, err, want)
		}
	}
	if _, err := ParseAddress("myreg.azurecr.co"); err == nil || !strings.Contains(err.Error(), "myreg.azurecr.de, myreg.private.example") {
		t.Errorf("error = %v, want the listed suffixes among the accepted forms", err)
	}

	t.Setenv(ACRSuffixesEnv, ".azurecr.de,bad suffix")
	if _, err := ParseAddress("myreg"); err == nil || !strings.Contains(err.Error(), ACRSuffixesEnv) {
		t.Errorf("an invalid %s entry should be reported, got %v", ACRSuffixesEnv, err)
	}
}

func TestKindString(t *testing.T) {
	if ACR.String() != "ACR" || GHCR.String() != "GHCR" || Kind(0).String() != "Kind(0)" {
		t.Errorf("Kind strings = %s, %s, %s", ACR, GHCR, Kind(0))
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name                 string
		err                  error
		permission, notFound bool
	}{
		{"forbidden", &ResponseError{StatusCode: 403}, true, false},
		{"unauthorized", &ResponseError{StatusCode: 401}, true, false},
		{"wrapped forbidden", fmt.Errorf("prune failed: %w", &ResponseError{StatusCode: 403}), true, false},
		{"not found", &ResponseError{StatusCode: 404}, false, true},
		{"wrapped not found", fmt.Errorf("listing: %w", &ResponseError{StatusCode: 404}), false, true},
		{"server error", &ResponseError{StatusCode: 500}, false, false},
		// Deletions report every failure, joined.
		{"joined forbidden", errors.Join(errors.New("boom"), fmt.Errorf("delete: %w", &ResponseError{StatusCode: 403})), true, false},
		{"joined not found", errors.Join(&ResponseError{StatusCode: 404}, context.Canceled), false, true},
		{"plain error", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, tt := range tests {
		if got := IsPermissionError(tt.err); got != tt.permission {
			t.Errorf("%s: IsPermissionError = %v, want %v", tt.name, got, tt.permission)
		}
		if got := IsNotFound(tt.err); got != tt.notFound {
			t.Errorf("%s: IsNotFound = %v, want %v", tt.name, got, tt.notFound)
		}
	}
}

func TestResponseErrorMessage(t *testing.T) {
	cause := errors.New("GET /v2/app: 403 Forbidden")
	err := &ResponseError{StatusCode: 403, Err: cause}
	if err.Error() != cause.Error() || !errors.Is(err, cause) {
		t.Errorf("ResponseError should present and wrap its cause: %v", err)
	}
	if got := (&ResponseError{StatusCode: 404}).Error(); got != "Not Found" {
		t.Errorf("ResponseError without a cause = %q, want the status text", got)
	}
}

func TestNewRejectsUnusableParallelism(t *testing.T) {
	// A limit of zero makes errgroup.Go block forever.
	for _, parallelism := range []int{0, -1} {
		if _, err := New(nil, nil, parallelism, nil); err == nil {
			t.Errorf("New should reject parallelism %d", parallelism)
		}
	}
	if _, err := New(nil, nil, 16, nil); err != nil {
		t.Errorf("valid parallelism rejected: %v", err)
	}
}
