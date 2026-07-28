package registry

import (
	"strings"
	"testing"
)

const imageManifest = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {"digest": "sha256:cfg", "size": 4096},
  "layers": [
    {"digest": "sha256:l1", "size": 100000},
    {"digest": "sha256:l2", "size": 200000}
  ]
}`

// TestParseManifestSizeIsDocumentOnly pins the contract the statistics rely
// on. Size once included the config blob, which the accounting then added
// again, inflating every reported repository size by one config per manifest.
func TestParseManifestSizeIsDocumentOnly(t *testing.T) {
	raw := []byte(imageManifest)
	m, err := parseManifest("myrepo", "sha256:abc", raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != uint64(len(raw)) {
		t.Errorf("Size = %d, want %d: Size is the manifest document alone, the config blob is counted separately", m.Size, len(raw))
	}
	if m.Config == nil || m.Config.Size != 4096 {
		t.Errorf("config descriptor = %+v, want size 4096", m.Config)
	}
	if len(m.Layers) != 2 {
		t.Errorf("layers = %+v, want 2", m.Layers)
	}
	if m.Repository != "myrepo" || m.Digest != "sha256:abc" {
		t.Errorf("addressed as %s@%s", m.Repository, m.Digest)
	}
}

func TestParseManifestRejectsBadDocuments(t *testing.T) {
	tests := []struct {
		name, raw, wantErr string
	}{
		{"malformed JSON", `{"schemaVersion": 2`, "failed to decode"},
		{"schema version 1", `{"schemaVersion": 1}`, "unsupported schema version"},
		{"no schema version", `{}`, "unsupported schema version"},
	}
	for _, tt := range tests {
		_, err := parseManifest("myrepo", "sha256:abc", []byte(tt.raw))
		if err == nil {
			t.Errorf("%s: expected an error", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %v, want it to mention %q", tt.name, err, tt.wantErr)
		}
	}
}

// TestParseManifestIndex checks that an index's children are read, since the
// pruner walks them to find orphans and dependencies.
func TestParseManifestIndex(t *testing.T) {
	const index = `{
	  "schemaVersion": 2,
	  "mediaType": "application/vnd.oci.image.index.v1+json",
	  "manifests": [
	    {"digest": "sha256:amd", "platform": {"architecture": "amd64", "os": "linux"}},
	    {"digest": "sha256:arm", "platform": {"architecture": "arm64", "os": "linux"}}
	  ]
	}`
	m, err := parseManifest("myrepo", "sha256:idx", []byte(index))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Manifests) != 2 {
		t.Fatalf("child manifests = %+v, want 2", m.Manifests)
	}
	if got := strings.Join(m.Architectures(), ","); got != "amd64,arm64" {
		t.Errorf("Architectures = %q", got)
	}
}
