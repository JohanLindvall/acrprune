package registry

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// NextPage resolves the next relation in Link headers against the response
// URL (RFC 8288). Authenticated pagination must stay on the original origin.
// Malformed or ambiguous links fail closed rather than truncate a listing.
func NextPage(headers []string, base *url.URL) (string, error) {
	var next string
	for _, header := range headers {
		for rest := strings.TrimSpace(header); rest != ""; {
			if !strings.HasPrefix(rest, "<") {
				return "", errors.New("invalid pagination Link header: expected <URL>")
			}
			target, tail, ok := strings.Cut(rest[1:], ">")
			if !ok || target == "" {
				return "", errors.New("invalid pagination Link header: missing URL or closing >")
			}
			params, following, valid := linkParameters(tail)
			if !valid || params["rel"] == "" {
				return "", errors.New("invalid pagination Link header: invalid relation parameters")
			}
			rest = following
			if !slices.ContainsFunc(strings.Fields(params["rel"]), func(rel string) bool { return strings.EqualFold(rel, "next") }) {
				continue
			}
			// An anchor changes the link's context. It cannot describe the
			// next page of this listing without further interpretation.
			if _, ok := params["anchor"]; ok {
				return "", errors.New("invalid pagination Link header: next link changes its context")
			}
			u, err := url.Parse(target)
			if err != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" {
				return "", errors.New("invalid pagination Link header: unsafe next page URL")
			}
			u = base.ResolveReference(u)
			if !SameOrigin(u, base) {
				return "", fmt.Errorf("refusing to follow the next page link away from %s", base.Host)
			}
			if next != "" && next != u.String() {
				return "", errors.New("invalid pagination Link header: multiple next pages")
			}
			next = u.String()
		}
	}
	return next, nil
}

// linkParameters consumes parameters through the next unquoted comma. Link
// parameters are not MIME parameters: rel* and rel*0 must not replace rel,
// and extensions may omit a value or repeat (for example, hreflang).
func linkParameters(s string) (params map[string]string, rest string, ok bool) {
	params = map[string]string{}
	s = strings.TrimSpace(s)
	for s != "" && s[0] != ',' {
		if s[0] != ';' {
			return nil, "", false
		}
		name, tail := linkToken(strings.TrimSpace(s[1:]))
		if name == "" {
			return nil, "", false
		}
		s = strings.TrimSpace(tail)
		var value string
		if strings.HasPrefix(s, "=") {
			s = strings.TrimSpace(s[1:])
			if !strings.HasPrefix(s, `"`) {
				value, s = linkToken(s)
				if value == "" {
					return nil, "", false
				}
			} else {
				s = s[1:]
				var unquoted strings.Builder
				for {
					if s == "" {
						return nil, "", false
					}
					c := s[0]
					s = s[1:]
					if c == '"' {
						break
					}
					if c == '\\' {
						if s == "" {
							return nil, "", false
						}
						c, s = s[0], s[1:]
					}
					if c < ' ' && c != '\t' || c == 0x7f {
						return nil, "", false
					}
					unquoted.WriteByte(c)
				}
				value = unquoted.String()
			}
		}
		name = strings.ToLower(name)
		if previous, exists := params[name]; exists {
			if (name == "rel" || name == "anchor") && previous != value {
				return nil, "", false
			}
		} else {
			params[name] = value
		}
		s = strings.TrimSpace(s)
	}
	return params, strings.TrimSpace(strings.TrimPrefix(s, ",")), true
}

// linkToken consumes an HTTP token, whose characters are ASCII only.
func linkToken(s string) (token, rest string) {
	for i := range len(s) {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return s[:i], s[i:]
	}
	return s, ""
}
