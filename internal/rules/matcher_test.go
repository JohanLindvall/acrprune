package rules

import (
	"fmt"
	"regexp"
	"slices"
	"testing"
)

func TestRepositoryMatcherPreservesFirstMatch(t *testing.T) {
	patterns := []string{`^team/`, `^app$`, `^my\.repo$`, `^app$`, `^other$`, `.*`, `^last$`}
	rules := make([]*RepoRule, len(patterns))
	for i, pattern := range patterns {
		rules[i] = &RepoRule{Repo: regexp.MustCompile(pattern)}
	}
	for n := range len(rules) + 1 {
		matcher := NewRepositoryMatcher(rules[:n])
		for _, name := range []string{"team/app", "app", "my.repo", "myXrepo", "other", "last", "unmatched"} {
			var want *RepoRule
			if i := slices.IndexFunc(rules[:n], func(r *RepoRule) bool { return r.Repo.MatchString(name) }); i >= 0 {
				want = rules[i]
			}
			if got := matcher.Match(name); got != want {
				t.Fatalf("%d rules, repository %q: got %v, want %v", n, name, got, want)
			}
		}
	}
}

func FuzzRepositoryMatcher(f *testing.F) {
	f.Add("app", "^app", "app.extra")
	f.Add("team/api", ".*", "team/api")
	f.Add("my.repo", "(?i)^MY", "myXrepo")
	f.Fuzz(func(t *testing.T, literal, pattern, repository string) {
		broad, err := regexp.Compile(pattern)
		if err != nil {
			return
		}
		exactRE, err := regexp.Compile("^" + regexp.QuoteMeta(literal) + "$")
		if err != nil {
			return
		}
		exact := &RepoRule{Repo: exactRE}
		other := &RepoRule{Repo: broad}
		for _, rules := range [][]*RepoRule{{exact, other}, {other, exact}, {exact, other, exact}} {
			var want *RepoRule
			for _, rule := range rules {
				if rule.Repo.MatchString(repository) {
					want = rule
					break
				}
			}
			if got := NewRepositoryMatcher(rules).Match(repository); got != want {
				t.Fatalf("indexed and sequential rules disagree for %q, pattern %q, repository %q", literal, pattern, repository)
			}
		}
	})
}

func BenchmarkRepositoryMatcher(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		rules := make([]*RepoRule, n)
		names := make([]string, n)
		for i := range rules {
			names[i] = fmt.Sprintf("repo%d", i)
			rules[i] = &RepoRule{Repo: regexp.MustCompile("^" + names[i] + "$")}
		}
		b.Run(fmt.Sprintf("indexed/%d", n), func(b *testing.B) {
			matcher := NewRepositoryMatcher(rules)
			b.ReportAllocs()
			for b.Loop() {
				for _, name := range names {
					if matcher.Match(name) == nil {
						b.Fatal("unmatched repository")
					}
				}
			}
		})
		b.Run(fmt.Sprintf("linear/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, name := range names {
					if slices.IndexFunc(rules, func(r *RepoRule) bool { return r.Repo.MatchString(name) }) < 0 {
						b.Fatal("unmatched repository")
					}
				}
			}
		})
	}
}
