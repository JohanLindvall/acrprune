package rules

import (
	"strings"
	"testing"
	"time"
)

// taggedRule is a rule file whose third line holds a tagged rule, so that the
// rule's own text starts at column 15 of line 3.
func taggedRule(rule string) string {
	return "[\n  {\"repo\": \".+\",\n   \"tagged\": [" + rule + "]}\n]"
}

// TestParseSpecsReportsPositions: errors in a long rule file used to carry no
// location, and some no hint of their cause — `time: unknown unit "days" in
// duration "30days"` — so finding the offending rule meant bisecting the file.
func TestParseSpecsReportsPositions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"syntax error", "[\n  {\"repo\": \".+\",}\n]", `line 2, column 17: invalid character '}' looking for beginning of object key string`},
		{"truncated", "[\n  {\"repo\": \".+\"}", `line 2, column 16: unexpected end of JSON input`},
		{"trailing content", `[{"repo": ".+"}] [{"repo": "other"}]`, `line 1, column 18: invalid character '[' after top-level value`},
		{"empty", " \n", `no rules: expected a JSON array of rules ([] for none)`},
		{"not an array", `{"repo": ".+"}`, `line 1, column 1: the rule file must be an array, not an object`},
		{"rule not an object", "[\n  1\n]", `line 2, column 3: rule 1 must be an object, not a number`},
		{"tagged rule not an object", taggedRule(`{}, "x"`), `line 3, column 19: tagged rule 2 must be an object, not a string`},
		{"list not an array", "[\n  {\"repo\": \".+\",\n   \"tagged\": {}}\n]", `line 3, column 14: "tagged" must be an array, not an object`},
		{"string field", "[\n  {\"repo\": true}\n]", `line 2, column 12: "repo" must be a string, not a boolean`},
		{"bool field", taggedRule(`{"keep": "no"}`), `line 3, column 24: "keep" must be true or false, not a string`},
		{"int field", taggedRule(`{"newest": 1.5}`), `line 3, column 26: "newest" must be an integer, not 1.5`},
		{"unknown field", taggedRule(`{"match_old": "30d"}`), `line 3, column 16: unknown field "match_old"`},
		{"bad duration", taggedRule(`{"match_older": "30days"}`), `line 3, column 31: "match_older": invalid duration "30days" (use e.g. "24h", "14d" or "2w")`},
		{"duration without unit", taggedRule(`{"match_older": 30}`), `line 3, column 31: "match_older": integer duration 30 counts nanoseconds; use a string instead (e.g. "24h", "14d" or "2w")`},
		{"duration of wrong type", taggedRule(`{"match_newer": {"days": 30}}`), `line 3, column 31: "match_newer": a duration must be a string (e.g. "24h", "14d" or "2w") or an integer number of nanoseconds`},
		{"leading blank lines", "\n\n  [{\"repo\": 1}]", `line 3, column 13: "repo" must be a string, not a number`},
		{"columns count characters", `[{"description": "é", "bogus": 1}]`, `line 1, column 23: unknown field "bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs, err := ParseSpecs(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("parsed %+v, want an error", specs)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q\n         want %q", err, tt.want)
			}
		})
	}
}

// TestParseSpecsRejectsDuplicateKeys: encoding/json keeps the last of a
// repeated key, so a hand-edited or merge-conflicted keep rule silently became
// a delete rule. Field names match case-insensitively, so a key repeated in
// another case overrides just the same.
func TestParseSpecsRejectsDuplicateKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"tagged rule", taggedRule(`{"tag": "^release-", "keep": true, "keep": false}`), `line 3, column 50: duplicate key "keep" (first set at line 3, column 36)`},
		{"other case", taggedRule(`{"keep": true, "Keep": false}`), `line 3, column 30: duplicate key "Keep" (keys ignore case, and "keep" is set at line 3, column 16)`},
		{"repository rule", "[\n  {\"repo\": \"^app$\",\n   \"repo\": \".+\"}\n]", `line 3, column 4: duplicate key "repo" (first set at line 2, column 4)`},
		{"list", `[{"repo": "^app$", "tagged": [], "tagged": [{"keep": false}]}]`, `line 1, column 34: duplicate key "tagged" (first set at line 1, column 20)`},
		{"inside a mistyped value", taggedRule(`{"keep": {"a": 1, "a": 2}}`), `line 3, column 33: duplicate key "a" (first set at line 3, column 25)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs, err := ParseSpecs(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("parsed %+v, want an error", specs)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q\n         want %q", err, tt.want)
			}
		})
	}

	// The same key in different objects is no duplicate.
	specs, err := ParseSpecs(strings.NewReader(`[
		{"repo": "^a$", "tagged": [{"tag": "^v", "keep": true}, {"keep": false}], "untagged": [{"keep": false}]},
		{"repo": "^b$", "tagged": [{"tag": "^v", "keep": true}]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || len(specs[0].Tagged) != 2 || *specs[0].Tagged[1].Keep {
		t.Errorf("unexpected parse result: %+v", specs)
	}
}

// TestParseSpecsAcceptsNullFields: null leaves a field unset, as it always has;
// the positioned checks must not start rejecting it.
func TestParseSpecsAcceptsNullFields(t *testing.T) {
	specs, err := ParseSpecs(strings.NewReader(`[{"repo": ".+", "description": null, "tagged": [{"match_older": null, "keep": null}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if tagged := specs[0].Tagged[0]; tagged.MatchOlderThan != nil || tagged.Keep != nil || specs[0].Description != nil {
		t.Errorf("null fields should stay unset: %+v", tagged)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"24h": 24 * time.Hour, "14d": 14 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "1h30m": 90 * time.Minute, "-1h": -time.Hour,
	} {
		if got, err := ParseDuration(in); got != want || err != nil {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "14", "30days", "d"} {
		_, err := ParseDuration(in)
		if err == nil || !strings.Contains(err.Error(), `invalid duration "`+in+`"`) || !strings.Contains(err.Error(), `"14d"`) {
			t.Errorf("ParseDuration(%q) error = %v, want one naming the value and showing examples", in, err)
		}
	}
}

// TestCompileNamesRules: Compile errors number rules from 1, as people count
// them, and quote the description when there is one.
func TestCompileNamesRules(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`[{"repo": "^a$"}, {"description": "prod releases", "repo": "^b$", "tagged": [{"tag": "^v"}, {"tag": "["}]}]`,
			`rule 2 ("prod releases"): tagged rule 2: invalid tag regex "["`},
		{`[{"repo": "["}]`, `rule 1: invalid repo regex "["`},
		{`[{"repo": "^a$"}, null]`, `rule 2: repository rule must not be null`},
		{`[{"repo": "^a$", "untagged": [{"keep": false}, null]}]`, `rule 1: untagged rule 2 must not be null`},
		{`[{"repo": "^a$", "untagged": [{"arch": "("}]}]`, `rule 1: untagged rule 1: invalid arch regex "("`},
	}
	for _, tt := range tests {
		specs, err := ParseSpecs(strings.NewReader(tt.input))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Compile(specs); err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want it to start with %q", tt.input, err, tt.want)
		}
	}
}
