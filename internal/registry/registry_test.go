package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

func TestLoginServer(t *testing.T) {
	tests := []struct{ name, want string }{
		{"myreg", "myreg.azurecr.io"},
		{"myreg.azurecr.cn", "myreg.azurecr.cn"},
		{"myreg.azurecr.us", "myreg.azurecr.us"},
	}
	for _, tt := range tests {
		if got := LoginServer(tt.name); got != tt.want {
			t.Errorf("LoginServer(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// fakePager pages through the given values, failing on a value of -1.
func fakePager(values []int) *runtime.Pager[int] {
	i := 0
	return runtime.NewPager(runtime.PagingHandler[int]{
		More: func(int) bool { return i < len(values) },
		Fetcher: func(ctx context.Context, _ *int) (int, error) {
			v := values[i]
			i++
			if v == -1 {
				return 0, errors.New("page fetch failed")
			}
			return v, nil
		},
	})
}

func TestForEachPage(t *testing.T) {
	var got []int
	err := forEachPage(context.Background(), fakePager([]int{1, 2, 3}), func(page int) error {
		got = append(got, page)
		return nil
	})
	if err != nil || len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("forEachPage = %v, %v", got, err)
	}

	// A paging error aborts the walk.
	got = nil
	err = forEachPage(context.Background(), fakePager([]int{1, -1, 3}), func(page int) error {
		got = append(got, page)
		return nil
	})
	if err == nil || len(got) != 1 {
		t.Errorf("paging error not propagated: %v, %v", got, err)
	}

	// An fn error aborts the walk too.
	boom := errors.New("boom")
	err = forEachPage(context.Background(), fakePager([]int{1, 2}), func(int) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("fn error not propagated: %v", err)
	}
}
