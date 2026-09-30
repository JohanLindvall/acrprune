package registry

import (
	"fmt"
	"regexp"
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
// an ACR login server (myreg.azurecr.cn, e.g. for a sovereign cloud) or a
// GHCR namespace (ghcr.io/myorg).
func ParseAddress(s string) (Address, error) {
	s = strings.TrimRight(s, "/")
	host, owner, hasPath := strings.Cut(s, "/")
	host = strings.ToLower(host)
	if strings.EqualFold(host, GHCRHost) {
		// Image references spell the owner in lowercase, whatever the case
		// of the GitHub account name.
		owner = strings.ToLower(owner)
		if !imageref.ValidOwner(owner) {
			return Address{}, fmt.Errorf("invalid registry %q: expected ghcr.io/<owner>, naming the user or organization that owns the packages", s)
		}
		return Address{Kind: GHCR, Host: GHCRHost, Owner: owner}, nil
	}
	if hasPath || len(host) > 253 || !hostPattern.MatchString(host) {
		return Address{}, fmt.Errorf("unsupported registry %q: expected an ACR registry name (myreg), an ACR login server (myreg.azurecr.io) or ghcr.io/<owner>", s)
	}
	if !strings.Contains(host, ".") {
		host += ".azurecr.io"
	}
	return Address{Kind: ACR, Host: host}, nil
}

// String returns what image references of the address's repositories start
// with, without the trailing slash: myreg.azurecr.io or ghcr.io/myorg.
func (a Address) String() string {
	if a.Owner == "" {
		return a.Host
	}
	return a.Host + "/" + a.Owner
}
