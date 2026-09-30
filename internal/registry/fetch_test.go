package registry

import (
	"io"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
)

const imageManifest = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {"digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000", "size": 4096},
  "layers": [
    {"digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111", "size": 100000},
    {"digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222", "size": 200000}
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
		{"unknown format", `{"schemaVersion":2,"mediaType":"unknown"}`, "unsupported media type"},
		{"bad dependency", `{"schemaVersion":2,"manifests":[{"digest":"../escape"}]}`, "invalid descriptor digest"},
		{"negative size", `{"schemaVersion":2,"layers":[{"digest":"sha256:` + strings.Repeat("a", 64) + `","size":-1}]}`, "negative descriptor size"},
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
	    {"digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333", "platform": {"architecture": "amd64", "os": "linux"}},
	    {"digest": "sha256:4444444444444444444444444444444444444444444444444444444444444444", "platform": {"architecture": "arm64", "os": "linux"}}
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

func TestVerify(t *testing.T) {
	content := []byte(imageManifest)
	for _, digest := range []godigest.Digest{godigest.SHA256.FromBytes(content), godigest.SHA512.FromBytes(content)} {
		if err := verify(digest.String(), content); err != nil {
			t.Errorf("verify(%s): %v", digest.Algorithm(), err)
		}
	}

	digest := godigest.FromBytes(content).String()
	if err := verify(digest, []byte("tampered")); err == nil {
		t.Error("content that does not hash to the digest must fail verification")
	}
	for _, bad := range []string{"", "sha256:short", "md5:d41d8cd98f00b204e9800998ecf8427e", "nocolon"} {
		if err := verify(bad, content); err == nil {
			t.Errorf("verify(%q) should reject the digest", bad)
		}
	}
}

// zeros reads as an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestReadDocument(t *testing.T) {
	data, err := ReadDocument(strings.NewReader(imageManifest))
	if err != nil || string(data) != imageManifest {
		t.Errorf("ReadDocument = %q, %v", data, err)
	}
	if _, err := ReadDocument(io.LimitReader(zeros{}, MaxDocumentSize)); err != nil {
		t.Errorf("a document of exactly the limit should be read: %v", err)
	}
	if _, err := ReadDocument(zeros{}); err == nil {
		t.Error("a runaway document should be refused")
	}
}
