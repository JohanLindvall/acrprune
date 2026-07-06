package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRefRoundtrip(t *testing.T) {
	ref := MakeRef("myrepo", "sha256:abc")
	if ref != "myrepo@sha256:abc" {
		t.Errorf("MakeRef = %q", ref)
	}
	repo, digest := ParseRef(ref)
	if repo != "myrepo" || digest != "sha256:abc" {
		t.Errorf("ParseRef = %q, %q", repo, digest)
	}
	repo, digest = ParseRef("no-at-sign")
	if repo != "" || digest != "" {
		t.Errorf("malformed ref should yield empty strings, got %q, %q", repo, digest)
	}
}

func TestTags(t *testing.T) {
	m := &Manifest{Azure: &azcontainerregistry.ManifestAttributes{
		Tags: []*string{to.Ptr("latest"), to.Ptr("v1")},
	}}
	if got := m.Tags(); strings.Join(got, ",") != "latest,v1" {
		t.Errorf("Tags = %v", got)
	}
	if got := (&Manifest{}).Tags(); len(got) != 0 {
		t.Errorf("manifest without attributes should yield no tags, got %v", got)
	}
}

func TestArchitectures(t *testing.T) {
	m := &Manifest{
		Azure: &azcontainerregistry.ManifestAttributes{
			Architecture: to.Ptr(azcontainerregistry.ArtifactArchitecture("unknown")),
		},
		OCIManifest: OCIManifest{
			Manifests: []v1.Descriptor{
				{Platform: &v1.Platform{Architecture: "amd64"}},
				{Platform: &v1.Platform{Architecture: "unknown"}},
				{Platform: &v1.Platform{Architecture: "arm64"}},
				{Platform: nil},
			},
		},
	}
	if got := m.Architectures(); strings.Join(got, ",") != "amd64,arm64" {
		t.Errorf("Architectures = %v, want [amd64 arm64]", got)
	}

	m.Azure.Architecture = to.Ptr(azcontainerregistry.ArtifactArchitectureArm64)
	if got := m.Architectures(); got[0] != "arm64" {
		t.Errorf("Architectures = %v, want arm64 first", got)
	}
}

func TestLogValueIncludesUpdated(t *testing.T) {
	now := time.Now()
	m := &Manifest{Ref: "r@d", Azure: &azcontainerregistry.ManifestAttributes{LastUpdatedOn: &now}}
	attrs := m.LogValue().Group()
	if len(attrs) != 2 || attrs[0].Key != "ref" || attrs[1].Key != "updated" {
		t.Errorf("LogValue = %v", attrs)
	}
}

func TestLocked(t *testing.T) {
	tests := []struct {
		name  string
		attrs *azcontainerregistry.ManifestWriteableProperties
		want  bool
	}{
		{"no attributes", nil, false},
		{"fully enabled", &azcontainerregistry.ManifestWriteableProperties{CanDelete: to.Ptr(true), CanWrite: to.Ptr(true)}, false},
		{"delete disabled", &azcontainerregistry.ManifestWriteableProperties{CanDelete: to.Ptr(false)}, true},
		{"write disabled", &azcontainerregistry.ManifestWriteableProperties{CanWrite: to.Ptr(false)}, true},
	}
	for _, tt := range tests {
		m := &Manifest{Azure: &azcontainerregistry.ManifestAttributes{ChangeableAttributes: tt.attrs}}
		if got := m.Locked(); got != tt.want {
			t.Errorf("%s: Locked = %v, want %v", tt.name, got, tt.want)
		}
	}
	if (&Manifest{}).Locked() {
		t.Error("manifest without attributes should not be locked")
	}
}

func TestLogValueIncludesLocked(t *testing.T) {
	m := &Manifest{Ref: "r@d", Azure: &azcontainerregistry.ManifestAttributes{
		ChangeableAttributes: &azcontainerregistry.ManifestWriteableProperties{CanDelete: to.Ptr(false)},
	}}
	found := false
	for _, a := range m.LogValue().Group() {
		if a.Key == "locked" {
			found = true
		}
	}
	if !found {
		t.Error("LogValue of a locked manifest should include locked=true")
	}
}

func TestCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub") // exercise MkdirAll in Put
	c := NewCache(dir)
	if c.Get("sha256:x") != nil {
		t.Error("empty cache should miss")
	}
	c.Put("sha256:x", []byte("data"))
	if string(c.Get("sha256:x")) != "data" {
		t.Error("cache should hit after Put")
	}
	c.Remove("sha256:x")
	if c.Get("sha256:x") != nil {
		t.Error("cache should miss after Remove")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cache dir should exist: %v", err)
	}
}

func TestNilCache(t *testing.T) {
	c := NewCache("")
	if c != nil {
		t.Fatal("empty dir should yield nil cache")
	}
	// All operations must be nil-safe no-ops.
	c.Put("d", []byte("x"))
	if c.Get("d") != nil {
		t.Error("nil cache should always miss")
	}
	c.Remove("d")
}
