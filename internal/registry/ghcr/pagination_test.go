package ghcr

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
)

// TestPruneGHCRRechecksBeforeDeleting: GitHub numbers the pages of a version
// listing. When another job deletes a version from a page already read, every
// later version moves up a place, and the first version of the next page is
// never listed. Here that is a release index, whose untagged platform images
// then look unreferenced to the rules. The pruner lists the package again
// before deleting anything, sees the index, and skips the package.
func TestPruneGHCRRechecksBeforeDeleting(t *testing.T) {
	f, _ := newFakeGitHub(t, true)
	old := time.Now().Add(-30 * 24 * time.Hour)
	config := f.pushBlob([]byte(`{"architecture":"amd64","os":"linux"}`))
	arm := f.push("app", imageDoc("arm", config), old)
	amd := f.push("app", imageDoc("amd", config), old)
	release := f.push("app", indexDoc(child(amd, "linux", "amd64"), child(arm, "linux", "arm64")), old, "1.0")
	pr2 := f.push("app", imageDoc("pr-2", config), old, "pr-2")
	pr1 := f.push("app", imageDoc("pr-1", config), old, "pr-1")
	// Two versions per page, newest first: pr-1, pr-2 | 1.0, amd | arm.
	b := f.backend(t, 2)

	// Once the first page is read and pr-1's manifest downloaded, another
	// job deletes pr-1, just before the second page is served.
	manifest := "/v2/" + testOwner + "/app/manifests/" + pr1
	downloaded := make(chan struct{})
	var once sync.Once
	var deleted atomic.Bool
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == manifest && r.Header.Get("Authorization") != "":
			f.serveRegistry(w, r, strings.TrimPrefix(r.URL.EscapedPath(), "/v2/"))
			once.Do(func() { close(downloaded) })
			return true
		case strings.HasSuffix(r.URL.Path, "/packages/container/app/versions") && r.URL.Query().Get("page") == "2" && !deleted.Swap(true):
			select {
			case <-downloaded:
			case <-time.After(10 * time.Second):
				t.Error("pr-1's manifest was never downloaded")
			}
			f.mu.Lock()
			f.packages["app"] = slices.DeleteFunc(f.packages["app"], func(v *fakeVersion) bool { return v.digest == pr1 })
			f.mu.Unlock()
		}
		return false
	}

	ruleSet := compile(t, `[{"repo": "^app$", "untagged": [{"match_older": "24h", "keep": false}], "tagged": [{"keep": true}]}]`)
	err := stackPruner(t, b, false).Prune(ctx, ruleSet)
	if !errors.Is(err, registry.ErrRepositoryChanged) {
		t.Errorf("error = %v, want the package reported as changed", err)
	}
	if got := f.requested("DELETE"); len(got) != 0 {
		t.Errorf("deleted %v from a listing that missed the release index", got)
	}
	want := []string{pr2, release, amd, arm}
	slices.Sort(want)
	if got := f.remaining("app"); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the release with its platform images: %v", got, want)
	}
}
