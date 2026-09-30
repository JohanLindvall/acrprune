package imageref

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	pin := "sha256:" + strings.Repeat("a", 64)
	for _, tt := range []struct{ in, repo, tag, digest string }{
		{"app", "app", "latest", ""}, {"team/app:v1", "team/app", "v1", ""},
		{"app@" + pin, "app", "", pin}, {"app:v1@" + pin, "app", "v1", pin},
		{"a.b/c__d--e:TAG_1", "a.b/c__d--e", "TAG_1", ""},
	} {
		r, tag, digest, err := Split(tt.in)
		if err != nil || r != tt.repo || tag != tt.tag || digest != tt.digest {
			t.Errorf("Split(%q) = %q, %q, %q, %v", tt.in, r, tag, digest, err)
		}
	}
	for _, bad := range []string{"", "..", "../app", "app//other", "App:v1", "app:", "app@", "app@sha256:short", "app:v1@" + pin + "@extra", "app:bad tag", "app:" + strings.Repeat("a", 129), strings.Repeat("a", 256)} {
		if _, _, _, err := Split(bad); err == nil {
			t.Errorf("accepted invalid reference %q", bad)
		}
	}
}

func TestValidOwner(t *testing.T) {
	for _, owner := range []string{"acme", "a-b", "my.org_1", strings.Repeat("a", 255)} {
		if !ValidOwner(owner) {
			t.Errorf("rejected valid owner %q", owner)
		}
	}
	for _, bad := range []string{"", "a/b", "..", "A", "-acme", "acme-", strings.Repeat("a", 256)} {
		if ValidOwner(bad) {
			t.Errorf("accepted invalid owner %q", bad)
		}
	}
}

func FuzzSplit(f *testing.F) {
	for _, s := range []string{"app", "team/app:v1", "../x", "app@sha256:" + strings.Repeat("a", 64)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		repo, tag, digest, err := Split(s)
		if err == nil && (!ValidRepository(repo) || tag == "" && digest == "") {
			t.Fatalf("unsafe parse: %q -> %q %q %q", s, repo, tag, digest)
		}
	})
}
