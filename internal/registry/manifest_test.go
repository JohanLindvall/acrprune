package registry

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRef(t *testing.T) {
	m := &Manifest{Repository: "myrepo", Attributes: Attributes{Digest: "sha256:abc"}}
	if got := m.Ref(); got != "myrepo@sha256:abc" {
		t.Errorf("Ref = %q", got)
	}
}

// TestHasTimestamp: a manifest the registry reported no timestamp for must not
// claim to have one, since age rules would read the zero time as "infinitely
// old" and delete it.
func TestHasTimestamp(t *testing.T) {
	now := time.Now()
	if m := (&Manifest{Attributes: Attributes{LastUpdated: now}}); !m.HasTimestamp() {
		t.Error("a manifest with a last-updated time should report a timestamp")
	}
	if (&Manifest{}).HasTimestamp() {
		t.Error("a manifest without a last-updated time should not report a timestamp")
	}
}

func TestArchitectures(t *testing.T) {
	m := &Manifest{
		Attributes: Attributes{Architecture: "unknown"},
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

	m.Architecture = "arm64"
	if got := m.Architectures(); got[0] != "arm64" {
		t.Errorf("Architectures = %v, want arm64 first", got)
	}
}

func TestOperatingSystems(t *testing.T) {
	m := &Manifest{
		Attributes: Attributes{OS: "linux"},
		OCIManifest: OCIManifest{
			Manifests: []v1.Descriptor{
				{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
				{Platform: &v1.Platform{OS: "windows", Architecture: "amd64"}},
				{Platform: &v1.Platform{OS: "unknown"}},
				{Platform: nil},
			},
		},
	}
	if got := m.OperatingSystems(); strings.Join(got, ",") != "linux,windows" {
		t.Errorf("OperatingSystems = %v, want [linux windows]", got)
	}
	if got := (&Manifest{}).OperatingSystems(); len(got) != 0 {
		t.Errorf("manifest without platforms should yield no OS, got %v", got)
	}
}

func TestLogValue(t *testing.T) {
	m := &Manifest{Repository: "r", Attributes: Attributes{Digest: "d", LastUpdated: time.Now()}}
	attrs := m.LogValue().Group()
	if len(attrs) != 2 || attrs[0].Key != "ref" || attrs[1].Key != "updated" {
		t.Errorf("LogValue = %v", attrs)
	}

	full := &Manifest{
		Repository: "r",
		Attributes: Attributes{Digest: "d", Tags: []string{"v1", "latest"}, Architecture: "arm64", Locked: true},
		Orphaned:   true,
	}
	keys := map[string]string{}
	for _, a := range full.LogValue().Group() {
		keys[a.Key] = a.Value.String()
	}
	for key, want := range map[string]string{"ref": "r@d", "archs": "arm64", "orphaned": "true", "locked": "true", "tags": "v1,latest"} {
		if keys[key] != want {
			t.Errorf("LogValue[%s] = %q, want %q (all: %v)", key, keys[key], want, keys)
		}
	}
	if _, ok := keys["updated"]; ok {
		t.Error("a manifest without a timestamp should not log one")
	}
}

func TestSubjectDigest(t *testing.T) {
	if got := (&Manifest{}).SubjectDigest(); got != "" {
		t.Errorf("an image's SubjectDigest = %q, want none", got)
	}
	byTag := &Manifest{TagSubject: "sha256:tagged"}
	if got := byTag.SubjectDigest(); got != "sha256:tagged" {
		t.Errorf("SubjectDigest = %q, want the subject the tags name", got)
	}
	// The OCI subject, recorded in the document itself, wins.
	byTag.Subject = &v1.Descriptor{Digest: "sha256:declared"}
	if got := byTag.SubjectDigest(); got != "sha256:declared" {
		t.Errorf("SubjectDigest = %q, want the OCI subject", got)
	}
}

func TestIsLocked(t *testing.T) {
	tests := []struct {
		name string
		m    Manifest
		want bool
	}{
		{"unlocked", Manifest{Attributes: Attributes{Tags: []string{"v1"}}}, false},
		{"tag locks loaded, none locked", Manifest{Attributes: Attributes{Tags: []string{"v1"}}, LockedTags: []string{}}, false},
		{"manifest locked", Manifest{Attributes: Attributes{Locked: true}}, true},
		{"tag locked", Manifest{Attributes: Attributes{Tags: []string{"v1"}}, LockedTags: []string{"v1"}}, true},
	}
	for _, tt := range tests {
		if got := tt.m.IsLocked(); got != tt.want {
			t.Errorf("%s: IsLocked = %v, want %v", tt.name, got, tt.want)
		}
		// The dry run's locked=true annotation covers tag locks too.
		logged := false
		for _, a := range tt.m.LogValue().Group() {
			logged = logged || a.Key == "locked" && a.Value.Bool()
		}
		if logged != tt.want {
			t.Errorf("%s: logged locked=%v, want %v", tt.name, logged, tt.want)
		}
	}
}
