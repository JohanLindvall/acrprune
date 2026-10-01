package registry

import (
	"errors"
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
	valid := `"sha256:` + strings.Repeat("a", 64) + `"`
	tests := []struct {
		name, raw, wantErr string
		unsupported        bool
	}{
		{"malformed JSON", `{"schemaVersion": 2`, "failed to decode", false},
		{"schema version 1", `{"schemaVersion": 1}`, "schema version 1 (Docker image manifest schema 1)", true},
		{"no schema version", `{}`, "schema version 0", true},
		{"unknown format", `{"schemaVersion":2,"mediaType":"application/vnd.oci.artifact.manifest.v1+json"}`, `media type "application/vnd.oci.artifact.manifest.v1+json"`, true},
		{"bad dependency", `{"schemaVersion":2,"manifests":[{"digest":"../escape"}]}`, "invalid descriptor digest", false},
		{"bad config", `{"schemaVersion":2,"config":{"digest":"sha256:short"}}`, "invalid descriptor digest", false},
		{"bad subject", `{"schemaVersion":2,"config":{"digest":` + valid + `},"subject":{"digest":"md5:x"}}`, "invalid descriptor digest", false},
		{"negative size", `{"schemaVersion":2,"layers":[{"digest":` + valid + `,"size":-1}]}`, "negative descriptor size", false},
	}
	for _, tt := range tests {
		_, err := parseManifest("myrepo", "sha256:abc", []byte(tt.raw))
		if err == nil {
			t.Errorf("%s: expected an error", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "myrepo@sha256:abc") {
			t.Errorf("%s: error = %v, want it to name the manifest and mention %q", tt.name, err, tt.wantErr)
		}
		if got := errors.Is(err, ErrUnsupportedManifest); got != tt.unsupported {
			t.Errorf("%s: errors.Is(ErrUnsupportedManifest) = %v, want %v", tt.name, got, tt.unsupported)
		}
	}
}

// TestCheckFormat: the format is judged from the document alone, ahead of
// digest verification; what is not JSON is left to verification to reject.
func TestCheckFormat(t *testing.T) {
	for raw, unsupported := range map[string]bool{
		`{"schemaVersion": 1, "signatures": []}`:                                                    true,
		`{"schemaVersion": 2, "mediaType": "application/vnd.oci.artifact.manifest.v1+json"}`:        true,
		`{"schemaVersion": 2, "mediaType": "application/vnd.docker.distribution.manifest.v2+json"}`: false,
		`{"schemaVersion": 2}`: false,
		`not json`:             false,
		`["schemaVersion", 1]`: false,
	} {
		err := checkFormat("app@sha256:abc", []byte(raw))
		if got := errors.Is(err, ErrUnsupportedManifest); got != unsupported || !unsupported && err != nil {
			t.Errorf("checkFormat(%s) = %v, want unsupported=%v", raw, err, unsupported)
		}
	}
}

func TestTagSchemeSubject(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	long := strings.Repeat("cd", 64)
	tests := []struct {
		name string
		tags []string
		want string
	}{
		{"cosign signature", []string{"sha256-" + hex + ".sig"}, "sha256:" + hex},
		{"attestation and SBOM", []string{"sha256-" + hex + ".att", "sha256-" + hex + ".sbom"}, "sha256:" + hex},
		{"referrers tag schema", []string{"sha256-" + hex}, "sha256:" + hex},
		{"sha512", []string{"sha512-" + long + ".sig"}, "sha512:" + long},
		{"untagged", nil, ""},
		{"ordinary tag too", []string{"sha256-" + hex + ".sig", "latest"}, ""},
		{"two subjects", []string{"sha256-" + hex + ".sig", "sha256-" + strings.Repeat("0", 64) + ".sig"}, ""},
		{"uppercase hex", []string{"sha256-" + strings.ToUpper(hex) + ".sig"}, ""},
		{"short hex", []string{"sha256-" + hex[:63] + ".sig"}, ""},
		{"sha512 length for sha256", []string{"sha256-" + long}, ""},
		{"unknown suffix", []string{"sha256-" + hex + ".cert"}, ""},
		{"unknown algorithm", []string{"md5-" + hex[:32] + ".sig"}, ""},
	}
	for _, tt := range tests {
		if got := tagSchemeSubject(tt.tags); got != tt.want {
			t.Errorf("%s: tagSchemeSubject(%v) = %q, want %q", tt.name, tt.tags, got, tt.want)
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

func FuzzParseManifest(f *testing.F) {
	for _, raw := range []string{imageManifest, `{"schemaVersion":2,"manifests":[]}`, `{"schemaVersion":1}`, `null`, `{"schemaVersion":2,"subject":{"size":-1}}`} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > MaxDocumentSize {
			t.Skip()
		}
		// Parser input includes cached and remote documents. Arbitrary
		// JSON, including malformed descriptors, must never panic.
		_, _ = parseManifest("app", godigest.FromBytes(raw).String(), raw)
	})
}
