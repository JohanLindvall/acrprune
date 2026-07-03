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
	spec := &RepoRuleSpec{Tagged: []*TaggedRuleSpec{{TagRegex: to.Ptr("[")}}}
	if _, err := spec.Compile(); err == nil {
		t.Error("invalid tag regex should fail to compile")
	}
	spec = &RepoRuleSpec{Untagged: []*UntaggedRuleSpec{{CommonRuleSpec: CommonRuleSpec{ArchitectureRegex: to.Ptr("[")}}}}
	if _, err := spec.Compile(); err == nil {
		t.Error("invalid arch regex should fail to compile")
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
		{"^my_repo.v2$", "my_repo.v2", true},
		{"^app.*$", "", false},
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

func TestKeepRulesFromImageList(t *testing.T) {
	input := strings.Join([]string{
		"myreg.azurecr.io/app:v1",
		"myreg.azurecr.io/app:v2",
		"otherreg.azurecr.io/app:v9", // wrong registry, skipped
		"myreg.azurecr.io/tools/db:latest",
		"not-an-image-line",
		"myreg.azurecr.io/no-tag",
	}, "\n")

	specs := KeepRulesFromImageList(strings.NewReader(input), "myreg")
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

func TestKeepRulesFromImageListFullLoginServer(t *testing.T) {
	specs := KeepRulesFromImageList(strings.NewReader("myreg.azurecr.cn/app:v1\n"), "myreg.azurecr.cn")
	if len(specs) != 1 || specs[0].RepoRegex != "^app$" {
		t.Errorf("full login server input not handled: %+v", specs)
	}
}
