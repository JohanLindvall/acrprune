package registry

import (
	"fmt"
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestIndexPlatformConstraints(t *testing.T) {
	for _, tt := range []struct {
		name       string
		descriptor *v1.Platform
		want       []string
	}{
		{"omitted", nil, []string{"linux/arm64"}},
		{"partial compatible", &v1.Platform{Architecture: "arm64"}, []string{"/arm64", "linux/arm64"}},
		{"partial conflicting", &v1.Platform{Architecture: "amd64"}, []string{"/amd64"}},
		{"complete conflicting", &v1.Platform{Architecture: "amd64", OS: "windows"}, []string{"windows/amd64"}},
		{"unknown attestation", &v1.Platform{Architecture: "unknown", OS: "unknown"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			leaf := &Manifest{Attributes: Attributes{Digest: "leaf", Architecture: "arm64", OS: "linux"}}
			root := &Manifest{Attributes: Attributes{Digest: "root"}, OCIManifest: OCIManifest{Manifests: []v1.Descriptor{{Digest: "leaf", Platform: tt.descriptor}}}}
			resolveIndexPlatforms(map[string]*Manifest{"root": root, "leaf": leaf})
			var got []string
			for platform := range root.Platforms() {
				got = append(got, platform.OS+"/"+platform.Architecture)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("platforms = %v, want %v", got, tt.want)
			}
		})
	}
}

func nestedPlatforms(depth int) map[string]*Manifest {
	manifests := make(map[string]*Manifest, depth)
	for i := range depth {
		d := fmt.Sprint(i)
		m := &Manifest{Attributes: Attributes{Digest: d}}
		if i+1 < depth {
			m.Manifests = []v1.Descriptor{{Digest: digest.Digest(fmt.Sprint(i + 1))}}
		} else {
			m.Architecture, m.OS = "arm64", "linux"
		}
		manifests[d] = m
	}
	return manifests
}

func TestDeepAndCyclicIndexPlatforms(t *testing.T) {
	const depth = 2000
	manifests := nestedPlatforms(depth)
	// A cycle must terminate and must not duplicate the same platform.
	manifests[fmt.Sprint(depth-1)].Manifests = []v1.Descriptor{{Digest: "0"}}
	resolveIndexPlatforms(manifests)
	for _, m := range manifests {
		if got := slices.Collect(m.Platforms()); len(got) != 1 || got[0].Architecture != "arm64" || got[0].OS != "linux" {
			t.Fatalf("%s resolved platforms: %v", m.Digest, got)
		}
	}
}

func BenchmarkIndexPlatforms(b *testing.B) {
	for _, depth := range []int{1000, 10000} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			manifests := nestedPlatforms(depth)
			for b.Loop() {
				resolveIndexPlatforms(manifests)
			}
		})
	}
}
