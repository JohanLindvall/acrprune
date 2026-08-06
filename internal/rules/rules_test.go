package rules

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
)

func TestDurationUnmarshalJSON(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{`"24h"`, 24 * time.Hour, false},
		{`"30d"`, 30 * 24 * time.Hour, false},
		{`"2w"`, 14 * 24 * time.Hour, false},
		{`"1h30m"`, 90 * time.Minute, false},
		{`3600000000000`, time.Hour, false},
		{`"bogus"`, 0, true},
		{`true`, 0, true},
	}
	for _, tt := range tests {
		var d Duration
		err := json.Unmarshal([]byte(tt.in), &d)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && d.Duration != tt.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tt.in, d.Duration, tt.want)
		}
	}
}

func TestCompileDefaults(t *testing.T) {
	rule, err := (&RepoRuleSpec{RepoRegex: "^foo$"}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if !rule.IgnoreMissingManifests {
		t.Error("IgnoreMissingManifests should default to true")
	}
	if rule.DeleteOrphanedManifests {
		t.Error("DeleteOrphanedManifests should default to false")
	}
	if rule.MustDeleteEverything {
		t.Error("MustDeleteEverything should default to false")
	}
}

func TestCompileCommonRule(t *testing.T) {
	common, err := (&CommonRuleSpec{}).compile()
	if err != nil {
		t.Fatal(err)
	}
	if !common.Keep {
		t.Error("Keep should default to true")
	}
	if common.Architecture != nil {
		t.Error("unset arch should compile to nil regexp")
	}

	common, err = (&CommonRuleSpec{ArchitectureRegex: to.Ptr(""), Keep: to.Ptr(false)}).compile()
	if err != nil {
		t.Fatal(err)
	}
	if common.Keep {
		t.Error("explicit keep=false should be honored")
	}
	if common.Architecture != nil {
		t.Error("empty arch regex should compile to nil regexp (match-all)")
	}
}

func TestCompileRejectsBadRegex(t *testing.T) {
	if _, err := (&RepoRuleSpec{RepoRegex: "["}).Compile(); err == nil {
		t.Error("invalid repo regex should fail to compile")
	}
	spec := &RepoRuleSpec{RepoRegex: ".+", Tagged: []*TaggedRuleSpec{{TagRegex: to.Ptr("[")}}}
	if _, err := spec.Compile(); err == nil {
		t.Error("invalid tag regex should fail to compile")
	}
	spec = &RepoRuleSpec{RepoRegex: ".+", Untagged: []*UntaggedRuleSpec{{CommonRuleSpec: CommonRuleSpec{ArchitectureRegex: to.Ptr("[")}}}}
	if _, err := spec.Compile(); err == nil {
		t.Error("invalid arch regex should fail to compile")
	}
	spec = &RepoRuleSpec{RepoRegex: ".+", Untagged: []*UntaggedRuleSpec{{CommonRuleSpec: CommonRuleSpec{OSRegex: to.Ptr("[")}}}}
	if _, err := spec.Compile(); err == nil {
		t.Error("invalid os regex should fail to compile")
	}
	spec = &RepoRuleSpec{RepoRegex: ".+", Tagged: []*TaggedRuleSpec{{CommonRuleSpec: CommonRuleSpec{DigestRegex: to.Ptr("[")}}}}
	if _, err := spec.Compile(); err == nil {
		t.Error("invalid digest regex should fail to compile")
	}
}

// TestCompileRejectsEmptyRepo guards a destructive default: the empty pattern
// is a valid regex matching every repository in the registry.
func TestCompileRejectsEmptyRepo(t *testing.T) {
	if _, err := (&RepoRuleSpec{}).Compile(); err == nil {
		t.Error("a rule without a repo pattern should fail to compile")
	}
}

func TestLiteralRepoName(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
		ok      bool
	}{
		{"^myrepo$", "myrepo", true},
		{"^team/service$", "team/service", true},
		{`^my_repo\.v2$`, "my_repo.v2", true}, // as produced by regexp.QuoteMeta
		{"^my-repo$", "my-repo", true},

		// An unescaped dot is a metacharacter: `^my_repo.v2$` also matches
		// `my_repoXv2`, so it must be resolved by listing the catalog rather
		// than addressed as the literal name `my_repo.v2`.
		{"^my_repo.v2$", "", false},
		{"^app.*$", "", false},
		{"^(a|b)$", "", false},
		{`^a\d$`, "", false}, // \d is a character class, not a literal
		{`^a\$`, "", false},  // trailing escape
		{"^$", "", false},
		{"myrepo", "", false},
		{"^myrepo", "", false},
		{".+", "", false},
	}
	for _, tt := range tests {
		rule, err := (&RepoRuleSpec{RepoRegex: tt.pattern}).Compile()
		if err != nil {
			t.Fatalf("Compile(%q): %v", tt.pattern, err)
		}
		got, ok := rule.LiteralRepoName()
		if got != tt.want || ok != tt.ok {
			t.Errorf("LiteralRepoName(%q) = %q, %v; want %q, %v", tt.pattern, got, ok, tt.want, tt.ok)
		}
		// Whatever is reported as a literal name must be a repository the
		// pattern actually matches, and only that one.
		if ok && !rule.Repo.MatchString(got) {
			t.Errorf("LiteralRepoName(%q) = %q, which the pattern does not match", tt.pattern, got)
		}
	}
}

// TestLiteralRepoNameMatchesQuoteMeta checks that every name the generator can
// emit is recognized, so generated rules never force a catalog listing.
func TestLiteralRepoNameMatchesQuoteMeta(t *testing.T) {
	for _, name := range []string{"app", "team/service", "my.repo", "a-b_c.d/e", "repo.v2"} {
		rule, err := (&RepoRuleSpec{RepoRegex: anchored(name)}).Compile()
		if err != nil {
			t.Fatal(err)
		}
		got, ok := rule.LiteralRepoName()
		if !ok || got != name {
			t.Errorf("LiteralRepoName(%q) = %q, %v; want %q, true", anchored(name), got, ok, name)
		}
	}
}

func TestParseSpecsRejectsUnknownFields(t *testing.T) {
	if _, err := ParseSpecs(strings.NewReader(`[{"bogus_field": true}]`)); err == nil {
		t.Error("unknown fields should be rejected")
	}
	specs, err := ParseSpecs(strings.NewReader(`[{"repo": ".+", "tagged": [{"tag": "^v1$", "keep": true}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || len(specs[0].Tagged) != 1 {
		t.Errorf("unexpected parse result: %+v", specs)
	}
}

// TestParseSpecsRejectsTrailingContent stops a rule file from being silently
// read as only its first array.
func TestParseSpecsRejectsTrailingContent(t *testing.T) {
	if _, err := ParseSpecs(strings.NewReader(`[{"repo": ".+"}] [{"repo": "other"}]`)); err == nil {
		t.Error("trailing content should be rejected")
	}
}

func TestKeepRulesFromImageList(t *testing.T) {
	input := strings.Join([]string{
		"myreg.azurecr.io/app:v1",
		"myreg.azurecr.io/app:v2",
		"otherreg.azurecr.io/app:v9", // wrong registry, skipped
		"myreg.azurecr.io/tools/db:latest",
		"not-an-image-line",
		"myreg.azurecr.io/no-tag",
	}, "\n")

	specs, err := KeepRulesFromImageList(strings.NewReader(input), "myreg")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 repo rules, got %d", len(specs))
	}

	app := specs[0]
	if app.RepoRegex != "^app$" {
		t.Errorf("rule 0 repo = %q", app.RepoRegex)
	}
	// v1 keep, v2 keep, catch-all delete
	if len(app.Tagged) != 3 {
		t.Fatalf("rule 0 tagged rules = %d, want 3", len(app.Tagged))
	}
	if *app.Tagged[0].TagRegex != "^v1$" || !*app.Tagged[0].Keep {
		t.Errorf("rule 0 tag 0 = %+v", app.Tagged[0])
	}
	if *app.Tagged[2].TagRegex != ".+" || *app.Tagged[2].Keep {
		t.Errorf("rule 0 catch-all = %+v", app.Tagged[2])
	}
	if len(app.Untagged) != 1 || *app.Untagged[0].Keep {
		t.Errorf("rule 0 untagged = %+v", app.Untagged)
	}

	if specs[1].RepoRegex != "^tools/db$" {
		t.Errorf("rule 1 repo = %q", specs[1].RepoRegex)
	}
}

// TestKeepRulesFromImageListGroupsRepositories covers an image list that is
// not sorted by repository. A second rule for a repository would never be
// reached — pruning applies only the first rule matching a repository — so its
// tags would fall through to the first rule's catch-all and be deleted while
// still running.
func TestKeepRulesFromImageListGroupsRepositories(t *testing.T) {
	input := strings.Join([]string{
		"myreg.azurecr.io/app:v1",
		"myreg.azurecr.io/other:x",
		"myreg.azurecr.io/app:v2",
		"myreg.azurecr.io/app:v1", // duplicate, e.g. the same image on two pods
	}, "\n")

	specs, err := KeepRulesFromImageList(strings.NewReader(input), "myreg")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected one rule per repository, got %d: %+v", len(specs), specs)
	}
	if specs[0].RepoRegex != "^app$" {
		t.Fatalf("rule 0 repo = %q, want ^app$ (first-seen order)", specs[0].RepoRegex)
	}

	var kept []string
	for _, tagged := range specs[0].Tagged {
		if *tagged.Keep {
			kept = append(kept, *tagged.TagRegex)
		}
	}
	if strings.Join(kept, ",") != "^v1$,^v2$" {
		t.Errorf("kept tags for app = %v, want both v1 and v2, deduplicated", kept)
	}
}

func TestKeepRulesFromImageListFullLoginServer(t *testing.T) {
	specs, err := KeepRulesFromImageList(strings.NewReader("myreg.azurecr.cn/app:v1\n"), "myreg.azurecr.cn")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].RepoRegex != "^app$" {
		t.Errorf("full login server input not handled: %+v", specs)
	}
}

func TestSplitImageRef(t *testing.T) {
	const prefix = "myreg.azurecr.io/"
	tests := []struct {
		line, repository, tag, digest string
		ok                            bool
	}{
		{"myreg.azurecr.io/app:v1", "app", "v1", "", true},
		{"myreg.azurecr.io/team/app:1.2.3", "team/app", "1.2.3", "", true},
		// A pinned digest must not be read as part of the tag; a reference
		// carrying both yields both.
		{"myreg.azurecr.io/app:v1@sha256:abc", "app", "v1", "sha256:abc", true},
		{"myreg.azurecr.io/app@sha256:abc", "app", "", "sha256:abc", true},
		{"myreg.azurecr.io/app", "", "", "", false},
		{"myreg.azurecr.io/app:", "", "", "", false},
		{"myreg.azurecr.io/app@", "", "", "", false},
		{"myreg.azurecr.io/:v1", "", "", "", false},
		{"otherreg.azurecr.io/app:v1", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, tt := range tests {
		repository, tag, digest, ok := splitImageRef(tt.line, prefix)
		if repository != tt.repository || tag != tt.tag || digest != tt.digest || ok != tt.ok {
			t.Errorf("splitImageRef(%q) = %q, %q, %q, %v; want %q, %q, %q, %v",
				tt.line, repository, tag, digest, ok, tt.repository, tt.tag, tt.digest, tt.ok)
		}
	}
}

// TestKeepRulesFromImageListDigestPinned covers pods that run digest-pinned
// images: the pin must be kept whether or not the manifest is tagged in the
// registry, so digest keep rules go in both the tagged and untagged lists.
// These references used to be silently ignored, so the generated catch-alls
// deleted running images.
func TestKeepRulesFromImageListDigestPinned(t *testing.T) {
	input := strings.Join([]string{
		"myreg.azurecr.io/app@sha256:pinned",
		"myreg.azurecr.io/app:v1",
		"myreg.azurecr.io/app@sha256:pinned", // duplicate
		"myreg.azurecr.io/app:v2@sha256:other",
	}, "\n")

	specs, err := KeepRulesFromImageList(strings.NewReader(input), "myreg")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(specs))
	}
	app := specs[0]

	var taggedDigests, untaggedDigests, tags []string
	for _, r := range app.Tagged {
		if r.DigestRegex != nil {
			taggedDigests = append(taggedDigests, *r.DigestRegex)
			if r.TagRegex != nil {
				t.Errorf("digest keep should not constrain the tag: %+v", r)
			}
		} else if r.TagRegex != nil && *r.Keep {
			tags = append(tags, *r.TagRegex)
		}
	}
	for _, r := range app.Untagged {
		if r.DigestRegex != nil {
			untaggedDigests = append(untaggedDigests, *r.DigestRegex)
		}
	}

	wantDigests := `^sha256:pinned$,^sha256:other$`
	if strings.Join(taggedDigests, ",") != wantDigests {
		t.Errorf("tagged digest keeps = %v, want %s (deduplicated, in order)", taggedDigests, wantDigests)
	}
	if strings.Join(untaggedDigests, ",") != wantDigests {
		t.Errorf("untagged digest keeps = %v, want %s", untaggedDigests, wantDigests)
	}
	// v2 came pinned; its tag is kept as well as its digest.
	if strings.Join(tags, ",") != "^v1$,^v2$" {
		t.Errorf("tag keeps = %v, want v1 and v2", tags)
	}

	// The catch-alls still close both lists.
	last := app.Tagged[len(app.Tagged)-1]
	if last.TagRegex == nil || *last.TagRegex != ".+" || *last.Keep {
		t.Errorf("tagged catch-all = %+v", last)
	}
	lastUntagged := app.Untagged[len(app.Untagged)-1]
	if lastUntagged.DigestRegex != nil || *lastUntagged.Keep {
		t.Errorf("untagged catch-all = %+v", lastUntagged)
	}

	if _, err := Compile(specs); err != nil {
		t.Errorf("generated rules should compile: %v", err)
	}
}

// TestKeepRulesCompile makes sure generated rules survive compilation, which
// now rejects an empty repo pattern.
func TestKeepRulesCompile(t *testing.T) {
	specs, err := KeepRulesFromImageList(strings.NewReader("myreg.azurecr.io/app:v1\n"), "myreg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(specs); err != nil {
		t.Errorf("generated rules should compile: %v", err)
	}
}
