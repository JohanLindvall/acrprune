package registry

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/JohanLindvall/acrprune/internal/imageref"
)

// Kind identifies the registry service an Address points at.
type Kind int

const (
	// ACR is Azure Container Registry.
	ACR Kind = iota + 1
	// GHCR is GitHub Container Registry.
	GHCR
)

func (k Kind) String() string {
	switch k {
	case ACR:
		return "ACR"
	case GHCR:
		return "GHCR"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

// GHCRHost is the login server of GitHub Container Registry.
const GHCRHost = "ghcr.io"

// ACRSuffixesEnv names the environment variable listing further login server
// suffixes to accept as ACR, comma-separated (for example .azurecr.de), for
// clouds whose suffix ParseAddress does not know.
const ACRSuffixesEnv = "ACRPRUNE_ACR_SUFFIXES"

// acrSuffixes are the login server suffixes of Azure Container Registry in the
// public cloud, Azure China and Azure Government.
var acrSuffixes = []string{".azurecr.io", ".azurecr.cn", ".azurecr.us"}

// hostPattern matches a DNS host name, loosely: it keeps a registry name from
// being something like ".." that would escape the cache directory.
var hostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

// Address locates the repositories a run operates on.
type Address struct {
	Kind Kind
	// Host is the login server, e.g. myreg.azurecr.io or ghcr.io.
	Host string
	// Owner is the GitHub user or organization whose packages are the
	// repositories on GHCR; it is empty on ACR.
	Owner string
}

// ParseAddress interprets a --registry value: an ACR registry name (myreg),
// an ACR login server (myreg.azurecr.io; .azurecr.cn or .azurecr.us in a
// sovereign cloud; or a suffix listed in the ACRSuffixesEnv variable) or a
// GHCR namespace (ghcr.io/myorg), optionally prefixed with https://.
//
// Anything else is refused rather than taken for an ACR login server: the
// Azure credentials acrprune authenticates with would be sent to it.
func ParseAddress(s string) (Address, error) {
	rest := s
	if len(rest) >= len("https://") && strings.EqualFold(rest[:len("https://")], "https://") {
		rest = rest[len("https://"):]
	}
	rest = strings.TrimRight(rest, "/")
	host, owner, hasPath := strings.Cut(rest, "/")
	host = strings.ToLower(host)
	if host == GHCRHost {
		// Image references spell the owner in lowercase, whatever the case
		// of the GitHub account name.
		owner = strings.ToLower(owner)
		if !imageref.ValidOwner(owner) {
			return Address{}, fmt.Errorf("invalid registry %q: expected ghcr.io/<owner>, naming the user or organization that owns the packages", s)
		}
		return Address{Kind: GHCR, Host: GHCRHost, Owner: owner}, nil
	}
	suffixes, err := acceptedSuffixes()
	if err != nil {
		return Address{}, err
	}
	if hasPath || len(host) > 253 || !hostPattern.MatchString(host) {
		return Address{}, unsupported(s, suffixes)
	}
	name, suffix, qualified := strings.Cut(host, ".")
	if !qualified {
		return Address{Kind: ACR, Host: name + acrSuffixes[0]}, nil
	}
	if !slices.Contains(suffixes, "."+suffix) {
		return Address{}, unsupported(s, suffixes)
	}
	return Address{Kind: ACR, Host: host}, nil
}

// unsupported returns the error for a --registry value that names no
// supported registry, listing the forms that do.
func unsupported(s string, suffixes []string) error {
	servers := make([]string, len(suffixes))
	for i, suffix := range suffixes {
		servers[i] = "myreg" + suffix
	}
	return fmt.Errorf("unsupported registry %q: expected an ACR registry name (myreg), an ACR login server (%s; set %s to accept other suffixes) or ghcr.io/<owner>",
		s, strings.Join(servers, ", "), ACRSuffixesEnv)
}

// acceptedSuffixes returns the login server suffixes accepted as ACR: those of
// Azure's public and sovereign clouds, and any listed in ACRSuffixesEnv.
func acceptedSuffixes() ([]string, error) {
	suffixes := slices.Clone(acrSuffixes)
	for entry := range strings.SplitSeq(os.Getenv(ACRSuffixesEnv), ",") {
		suffix := strings.ToLower(strings.TrimSpace(entry))
		if suffix == "" {
			continue
		}
		suffix = "." + strings.TrimPrefix(suffix, ".")
		if !hostPattern.MatchString(suffix[1:]) {
			return nil, fmt.Errorf("invalid %s entry %q: expected a DNS suffix such as .azurecr.de", ACRSuffixesEnv, entry)
		}
		if !slices.Contains(suffixes, suffix) {
			suffixes = append(suffixes, suffix)
		}
	}
	return suffixes, nil
}

// String returns what image references of the address's repositories start
// with, without the trailing slash: myreg.azurecr.io or ghcr.io/myorg.
func (a Address) String() string {
	if a.Owner == "" {
		return a.Host
	}
	return a.Host + "/" + a.Owner
}
