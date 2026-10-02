package registry

import (
	"net/url"
	"strings"
	"testing"
)

func TestNextPageLinks(t *testing.T) {
	base, _ := url.Parse("https://registry.example/acr/v1/app/_tags?n=2")
	for _, test := range []struct {
		name    string
		headers []string
		want    string
		bad     bool
	}{
		{"none", nil, "", false},
		{"relative query", []string{`<?n=2&last=v1>; rel=next`}, "https://registry.example/acr/v1/app/_tags?n=2&last=v1", false},
		{"multiple fields", []string{`</previous>; rel=prev`, `</next>; rel="next last"`}, "https://registry.example/next", false},
		{"commas", []string{`</next?cursor=a,b;c>; title="a, b; c"; rel=next, </last>; rel=last`}, "https://registry.example/next?cursor=a,b;c", false},
		{"quoted escape", []string{`</next>; title="a\",b"; REL="NEXT"`}, "https://registry.example/next", false},
		{"extension cannot override relation", []string{`</next>; rel=next; rel*=UTF-8''prev`}, "https://registry.example/next", false},
		{"continuation cannot override relation", []string{`</next>; rel=next; rel*0=prev`}, "https://registry.example/next", false},
		{"extension attributes", []string{`</next>; rel=next; flag; title*=UTF-8'de'n%c3%a4chstes; hreflang=en; hreflang=de`}, "https://registry.example/next", false},
		{"absolute", []string{`<https://registry.example/next>; rel=next`}, "https://registry.example/next", false},
		{"last page", []string{`</previous>; rel=prev`}, "", false},
		{"no brackets", []string{`/next; rel=next`}, "", true},
		{"unclosed target", []string{`</next; rel=next`}, "", true},
		{"empty target", []string{`<>; rel=next`}, "", true},
		{"no relation", []string{`</next>`}, "", true},
		{"unclosed quote", []string{`</next>; rel="next`}, "", true},
		{"incomplete escape", []string{`</next>; rel="next\`}, "", true},
		{"invalid parameter name", []string{`</next>; (rel)=next`}, "", true},
		{"missing parameter", []string{`</next>; rel=next;`}, "", true},
		{"conflicting relations", []string{`</next>; rel=next; rel=prev`}, "", true},
		{"extended relation only", []string{`</next>; rel*=UTF-8''next`}, "", true},
		{"control in quoted value", []string{"</next>; rel=\"next\"; title=\"bad\x01value\""}, "", true},
		{"ambiguous", []string{`</one>; rel=next, </two>; rel=next`}, "", true},
		{"foreign", []string{`<https://elsewhere.example/next?secret=hidden>; rel=next`}, "", true},
		{"protocol relative", []string{`<//elsewhere.example/next>; rel=next`}, "", true},
		{"downgrade", []string{`<http://registry.example/next>; rel=next`}, "", true},
		{"port", []string{`<https://registry.example:8443/next>; rel=next`}, "", true},
		{"userinfo", []string{`<https://hidden@registry.example/next>; rel=next`}, "", true},
		{"fragment", []string{`</next#hidden>; rel=next`}, "", true},
		{"anchor", []string{`</next>; rel=next; anchor="/other"`}, "", true},
		{"bad escape", []string{`</%zzhidden>; rel=next`}, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NextPage(test.headers, base)
			if (err != nil) != test.bad || got != test.want {
				t.Fatalf("NextPage = %q, %v; want %q, bad=%t", got, err, test.want, test.bad)
			}
			if err != nil && strings.Contains(err.Error(), "hidden") {
				t.Fatal("error exposed URL credentials or query parameters")
			}
		})
	}
}

func FuzzNextPage(f *testing.F) {
	for _, seed := range []string{"", `</next>; rel=next`, `</next?a,b>; rel="next last"`, `broken`, `<https://evil.example/>; rel=next`, `</next>; rel=next; rel*=UTF-8''prev`, `</next>; title="a\",b"; REL="NEXT"`} {
		f.Add(seed)
	}
	base, _ := url.Parse("https://registry.example/packages?page=1")
	f.Fuzz(func(t *testing.T, header string) {
		next, err := NextPage([]string{header}, base)
		if err != nil || next == "" {
			return
		}
		u, err := url.Parse(next)
		if err != nil || !SameOrigin(u, base) || u.User != nil || u.Fragment != "" {
			t.Fatalf("unsafe next page %q: %v", next, err)
		}
	})
}
