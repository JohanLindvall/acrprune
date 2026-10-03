package explore

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/JohanLindvall/crprune/internal/rules"
	presets "github.com/JohanLindvall/crprune/rules"
)

// RuleFile is one independently selectable rule set. Invalid files remain in
// the catalog with Err set so they can be inspected, but never selected.
type RuleFile struct {
	Source string
	Rules  []*rules.RepoRule
	Err    error
}

// DiscoverRules loads bundled examples and JSON files directly in directory.
// Identical local copies of bundled examples are omitted. Modified copies
// remain separate choices, with their source shown explicitly.
func DiscoverRules(directory string) []RuleFile {
	var catalog []RuleFile
	bundled := map[string][]byte{}
	entries, err := presets.Files.ReadDir(".")
	if err != nil {
		catalog = append(catalog, RuleFile{Source: "bundled rules", Err: err})
	}
	for _, entry := range entries {
		data, err := presets.Files.ReadFile(entry.Name())
		bundled[entry.Name()] = data
		catalog = append(catalog, compileRuleFile("bundled:"+entry.Name(), data, err))
	}
	entries, err = os.ReadDir(directory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		catalog = append(catalog, RuleFile{Source: directory, Err: err})
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		source := filepath.Join(directory, entry.Name())
		data, err := readRuleFile(source)
		if original, ok := bundled[entry.Name()]; ok && err == nil && bytes.Equal(original, data) {
			continue
		}
		catalog = append(catalog, compileRuleFile(source, data, err))
	}
	return catalog
}

// Interactive rule loading accepts regular files, including symlinks to
// them. Opening a named pipe could otherwise hang discovery or prevent an
// active load from finishing when the user cancels the explorer.
func readRuleFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("rule source %q is not a regular file", path)
	}
	return os.ReadFile(path)
}

func compileRuleFile(source string, data []byte, err error) RuleFile {
	file := RuleFile{Source: source, Err: err}
	if err == nil {
		var specs []*rules.RepoRuleSpec
		specs, file.Err = rules.ParseSpecs(bytes.NewReader(data))
		if file.Err == nil {
			file.Rules, file.Err = rules.Compile(specs)
		}
	}
	return file
}

func (a *app) rememberRules(file RuleFile) {
	i := slices.IndexFunc(a.opts.RuleFiles, func(f RuleFile) bool { return f.Source == file.Source })
	if i < 0 {
		a.opts.RuleFiles = append(a.opts.RuleFiles, file)
	} else {
		a.opts.RuleFiles[i] = file
	}
}

func (a *app) chooseRules(req *request, scope string) {
	a.panel, a.status, a.ruleCursor, a.ruleOffset = "rule-picker", "", 0, 0
	a.ruleRequest, a.ruleScope = req, scope
	if c := a.opts.Client; c != nil {
		if i := slices.IndexFunc(a.opts.RuleFiles, func(f RuleFile) bool { return f.Source == c.RuleSource }); i >= 0 {
			a.ruleCursor = i
		}
	}
}

func ruleDetails(file RuleFile) []string {
	lines := []string{file.Source}
	if file.Err != nil {
		return append(lines, "Cannot select this file: "+file.Err.Error())
	}
	if len(file.Rules) == 0 {
		return append(lines, "Empty rule set; no images will be selected.")
	}
	for _, rule := range file.Rules {
		line := "Repositories: " + rule.Repo.String()
		if rule.Description != "" {
			line += " — " + rule.Description
		}
		lines = append(lines, line)
	}
	return append(lines, rules.Warnings(file.Rules)...)
}

func (a *app) manualRules() {
	a.panel, a.input, a.status = "rules", "", ""
	a.ruleRequest = nil
	if c := a.opts.Client; c != nil && !strings.HasPrefix(c.RuleSource, "bundled:") {
		a.input = c.RuleSource
	}
}
