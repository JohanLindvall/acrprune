package rules

// RepositoryMatcher finds the first matching rule without rescanning a
// generated inventory's literal repository rules. Rules and their regexes must
// remain unchanged while the matcher is in use. Concurrent lookups are safe.
type RepositoryMatcher struct {
	rules    []*RepoRule
	literals map[string]int
	patterns []int
}

// NewRepositoryMatcher indexes literal rules and preserves the positions of
// regex rules so an earlier broad pattern still wins over a later literal.
func NewRepositoryMatcher(ruleSet []*RepoRule) *RepositoryMatcher {
	m := &RepositoryMatcher{rules: ruleSet, literals: make(map[string]int)}
	for i, rule := range ruleSet {
		if name, ok := rule.LiteralRepoName(); ok {
			if _, exists := m.literals[name]; !exists {
				m.literals[name] = i
			}
		} else {
			m.patterns = append(m.patterns, i)
		}
	}
	return m
}

// Match returns the first rule matching repository, or nil. An all-literal
// inventory needs one map lookup per repository instead of a linear scan.
func (m *RepositoryMatcher) Match(repository string) *RepoRule {
	first, found := m.literals[repository]
	if !found {
		first = len(m.rules)
	}
	for _, i := range m.patterns {
		if i >= first {
			break
		}
		if m.rules[i].Repo.MatchString(repository) {
			return m.rules[i]
		}
	}
	if first < len(m.rules) {
		return m.rules[first]
	}
	return nil
}
