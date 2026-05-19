package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/go-git/go-git/v5"
)

const bumpConfigPath = ".bump.toml"

type bumpConfig struct {
	Files []fileEntry `toml:"file"`
}

type fileEntry struct {
	Path    string        `toml:"path"`
	Format  string        `toml:"format"`
	Replace []replaceRule `toml:"replace"`
}

type replaceRule struct {
	Match    string `toml:"match"`
	Template string `toml:"template"`
}

// presets maps a named format to its replace rules. {auto} preserves whatever
// v-prefix convention the existing line uses (e.g. version = "1.0.0" stays
// bare; version = "v1.0.0" stays prefixed). Add new ecosystems here.
var presets = map[string][]replaceRule{
	"cargo": {
		{Match: `^version\s*=\s*"[^"]*"`, Template: `version = "{auto}"`},
	},
	"npm": {
		// Preserves indent via $1. Replaces every "version": "..." entry —
		// in a normal package.json the top-level key is the only one.
		{Match: `^(\s*)"version"\s*:\s*"[^"]*"`, Template: `${1}"version": "{auto}"`},
	},
	"chart": {
		{Match: `^version:\s*.*$`, Template: `version: {auto}`},
		{Match: `^appVersion:\s*.*$`, Template: `appVersion: "{auto}"`},
	},
}

// vPrefixedInMatch detects whether the matched text contains a v-prefixed
// version string (e.g. "v1.2.3"). Used by the {auto} template variable to
// preserve the existing prefix convention of each match.
var vPrefixedInMatch = regexp.MustCompile(`\bv\d+\.\d+(\.\d+)?`)

func detectVPrefix(match []byte) bool {
	return vPrefixedInMatch.Match(match)
}

func knownPresets() string {
	names := make([]string, 0, len(presets))
	for k := range presets {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// loadBumpConfig returns (nil, nil) when the file is absent — bump still works
// without a config, and absence shouldn't be treated as an error.
func loadBumpConfig(path string) (*bumpConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg bumpConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// resolveRules returns the effective replace rules for a file entry. It errors
// on missing path, unknown format, or ambiguous configuration (both/neither
// format and replace).
func resolveRules(entry fileEntry) ([]replaceRule, error) {
	if entry.Path == "" {
		return nil, fmt.Errorf("file entry is missing path")
	}
	hasFormat := entry.Format != ""
	hasReplace := len(entry.Replace) > 0
	if hasFormat && hasReplace {
		return nil, fmt.Errorf("file %q: cannot set both 'format' and 'replace'", entry.Path)
	}
	if !hasFormat && !hasReplace {
		return nil, fmt.Errorf("file %q: must set either 'format' or 'replace'", entry.Path)
	}
	if hasFormat {
		rules, ok := presets[entry.Format]
		if !ok {
			return nil, fmt.Errorf("file %q: unknown format %q (known: %s)", entry.Path, entry.Format, knownPresets())
		}
		return rules, nil
	}
	return entry.Replace, nil
}

// expandTemplate substitutes {version}, {tag}, {major}, {minor}, {patch} in the
// template. Regex backreferences ($1, ${1}) are left intact for the subsequent
// regexp.ReplaceAll call.
func expandTemplate(template, version string) string {
	bare := stripVPrefix(version)
	tag := normalizeVersion(version)
	maj, min, pat := splitSemver(bare)
	r := strings.NewReplacer(
		"{version}", bare,
		"{tag}", tag,
		"{major}", maj,
		"{minor}", min,
		"{patch}", pat,
	)
	return r.Replace(template)
}

// splitSemver returns major/minor/patch parts of a bare semver. Any prerelease
// or build suffix is stripped from the patch component.
func splitSemver(bare string) (string, string, string) {
	parts := strings.SplitN(bare, ".", 3)
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	patch := parts[2]
	if i := strings.IndexAny(patch, "-+"); i >= 0 {
		patch = patch[:i]
	}
	return parts[0], parts[1], patch
}

// applyReplacements runs every rule against content. Each rule must match at
// least once; a zero-match rule is treated as an error (silent misses are the
// failure mode we most want to avoid). Every match for a rule is replaced.
//
// Patterns are compiled in multi-line mode: ^ and $ anchor to line boundaries,
// matching the natural reading of patterns like ^version: .*$ in config files.
//
// {auto} is resolved per-match: if the matched text contains a v-prefixed
// version it expands to {tag}, otherwise to {version}.
func applyReplacements(content []byte, rules []replaceRule, version string) ([]byte, error) {
	bare := stripVPrefix(version)
	tag := normalizeVersion(version)

	out := content
	for _, rule := range rules {
		re, err := regexp.Compile("(?m)" + rule.Match)
		if err != nil {
			return nil, fmt.Errorf("compile %q: %w", rule.Match, err)
		}
		matches := re.FindAllSubmatchIndex(out, -1)
		if len(matches) == 0 {
			return nil, fmt.Errorf("no match for pattern %q", rule.Match)
		}

		var buf bytes.Buffer
		lastEnd := 0
		for _, idx := range matches {
			start, end := idx[0], idx[1]
			buf.Write(out[lastEnd:start])

			tmpl := rule.Template
			if strings.Contains(tmpl, "{auto}") {
				auto := bare
				if detectVPrefix(out[start:end]) {
					auto = tag
				}
				tmpl = strings.ReplaceAll(tmpl, "{auto}", auto)
			}
			tmpl = expandTemplate(tmpl, version)

			// re.Expand handles regex backreferences like $1 / ${1}.
			buf.Write(re.Expand(nil, []byte(tmpl), out, idx))
			lastEnd = end
		}
		buf.Write(out[lastEnd:])
		out = buf.Bytes()
	}
	return out, nil
}

// validateConfig runs before any disk writes so structural errors (unknown
// preset, missing path, etc.) abort the bump before partial state is created.
func validateConfig(cfg *bumpConfig) error {
	if cfg == nil {
		return nil
	}
	for _, entry := range cfg.Files {
		if _, err := resolveRules(entry); err != nil {
			return err
		}
	}
	return nil
}

// applyBumpConfig returns the number of files actually modified. A file whose
// content already matches the target version is skipped (idempotent).
func applyBumpConfig(repo *git.Repository, cfg *bumpConfig, dryRun bool, output io.Writer, newVersion string) (int, error) {
	if cfg == nil {
		return 0, nil
	}
	updated := 0
	for _, entry := range cfg.Files {
		rules, err := resolveRules(entry)
		if err != nil {
			return updated, err
		}
		content, err := os.ReadFile(entry.Path)
		if err != nil {
			return updated, fmt.Errorf("read %s: %w", entry.Path, err)
		}
		newContent, err := applyReplacements(content, rules, newVersion)
		if err != nil {
			return updated, fmt.Errorf("file %s: %w", entry.Path, err)
		}
		if bytes.Equal(content, newContent) {
			continue
		}
		fmt.Fprintf(output, "Updating %s\n", entry.Path)
		updated++
		if dryRun {
			continue
		}
		if err := os.WriteFile(entry.Path, newContent, 0644); err != nil {
			return updated, fmt.Errorf("write %s: %w", entry.Path, err)
		}
		if err := add(repo, entry.Path); err != nil {
			return updated, fmt.Errorf("add %s: %w", entry.Path, err)
		}
	}
	return updated, nil
}
