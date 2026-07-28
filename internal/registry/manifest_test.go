package registry

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRef(t *testing.T) {
	m := &Manifest{Repository: "myrepo", Digest: "sha256:abc"}
	if got := m.Ref(); got != "myrepo@sha256:abc" {
		t.Errorf("Ref = %q", got)
	}
}

func TestTags(t *testing.T) {
	m := &Manifest{Azure: &azcontainerregistry.ManifestAttributes{
		Tags: []*string{to.Ptr("latest"), nil, to.Ptr("v1")},
	}}
	if got := m.Tags(); strings.Join(got, ",") != "latest,v1" {
		t.Errorf("Tags = %v", got)
	}
	if got := (&Manifest{}).Tags(); len(got) != 0 {
		t.Errorf("manifest without attributes should yield no tags, got %v", got)
	}
}

func TestLastUpdated(t *testing.T) {
	now := time.Now()
	m := &Manifest{Azure: &azcontainerregistry.ManifestAttributes{LastUpdatedOn: &now}}
	if !m.HasTimestamp() || !m.LastUpdated().Equal(now) {
		t.Errorf("LastUpdated = %v, HasTimestamp = %v", m.LastUpdated(), m.HasTimestamp())
	}

	// A manifest the registry reported no timestamp for must not panic and
	// must not claim to have one: age rules would read the zero time as
	// "infinitely old" and delete it.
	for _, m := range []*Manifest{
		{},
		{Azure: &azcontainerregistry.ManifestAttributes{}},
		{Azure: &azcontainerregistry.ManifestAttributes{LastUpdatedOn: &time.Time{}}},
	} {
		if m.HasTimestamp() {
			t.Errorf("%+v should not report a timestamp", m.Azure)
		}
		if !m.LastUpdated().IsZero() {
			t.Errorf("LastUpdated = %v, want zero time", m.LastUpdated())
		}
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
				{Platform: &v1.Platform{Architecture: "amd64"}}, // duplicate
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
	m := &Manifest{Repository: "r", Digest: "d", Azure: &azcontainerregistry.ManifestAttributes{LastUpdatedOn: &now}}
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
	m := &Manifest{Repository: "r", Digest: "d", Azure: &azcontainerregistry.ManifestAttributes{
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

func TestIsPermissionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"forbidden", &azcore.ResponseError{StatusCode: 403}, true},
		{"unauthorized", &azcore.ResponseError{StatusCode: 401}, true},
		{"wrapped forbidden", fmt.Errorf("prune failed: %w", &azcore.ResponseError{StatusCode: 403}), true},
		{"not found", &azcore.ResponseError{StatusCode: 404}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		if got := IsPermissionError(tt.err); got != tt.want {
			t.Errorf("%s: IsPermissionError = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNewRejectsUnusableLimits(t *testing.T) {
	tests := []struct {
		name                  string
		pageSize, parallelism int
	}{
		// A limit of zero makes errgroup.Go block forever.
		{"zero parallelism", 250, 0},
		{"negative parallelism", 250, -1},
		{"zero page size", 0, 16},
		{"page size overflowing int32", 1 << 40, 16},
	}
	for _, tt := range tests {
		if _, err := New(nil, nil, tt.pageSize, tt.parallelism, nil); err == nil {
			t.Errorf("%s: New should have failed", tt.name)
		}
	}
	if _, err := New(nil, nil, 250, 16, nil); err != nil {
		t.Errorf("valid limits rejected: %v", err)
	}
}
