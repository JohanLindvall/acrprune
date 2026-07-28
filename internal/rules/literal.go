package rules

import "strings"

// regexMeta are the characters regexp.QuoteMeta escapes, i.e. every character
// that means something other than itself inside a pattern.
const regexMeta = `\.+*?()|[]{}^$`

// repoNameChars reports whether c can appear in an ACR repository name.
func repoNameChars(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-' || c == '/'
}

// LiteralRepoName reports whether the rule's repository pattern is an anchored
// plain name (`^team/svc$`, or `^my\.repo$` as produced by regexp.QuoteMeta)
// and returns that name, allowing callers to address the repository directly
// instead of listing the whole registry.
//
// Escapes are resolved, but a pattern holding an *active* metacharacter is
// rejected: `^my.repo$` also matches `myXrepo`, so treating it as the literal
// name `my.repo` would make a rule match different repositories depending on
// whether some other rule in the file happened to force a catalog listing.
func (r *RepoRule) LiteralRepoName() (string, bool) {
	body, ok := strings.CutPrefix(r.Repo.String(), "^")
	if !ok {
		return "", false
	}
	body, ok = strings.CutSuffix(body, "$")
	if !ok {
		return "", false
	}

	var name strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\\' {
			// Only escaped metacharacters stand for themselves; \d, \w and
			// \p{…} are character classes.
			i++
			if i == len(body) || strings.IndexByte(regexMeta, body[i]) < 0 {
				return "", false
			}
			c = body[i]
		} else if strings.IndexByte(regexMeta, c) >= 0 {
			return "", false
		}
		if !repoNameChars(c) {
			return "", false
		}
		name.WriteByte(c)
	}
	if name.Len() == 0 {
		return "", false
	}
	return name.String(), true
}
