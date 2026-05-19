# bump

A simple tool to bump version numbers in git. Written as replacement for standard-version, which is deprecated.

## Installation

```sh
go install github.com/perbu/bump@latest
```

## What it does

bump starts out by reading all the tags from git. It will discard everything that doesn't look like a version
number (v1.2.3, 1.2.3, 1.2.3-alpha.1, etc). It will then sort the versions and pick the highest one. If there are no
tags, it will fail.

It will then increment ("bump") the version number according to the command line arguments. If no arguments are given
it will default to bumping the patch version.

It will then look for files named `.version`. If any such files are found in the repository their content will be
replaced with the new version number.

These files will then be added to git and committed with a message that includes the new version number. bump will try
to access the ssh-agent to sign the commit.

Finally, it will create a new tag in git with the bumped version number.

### Version prefix handling

bump preserves the `v` prefix convention of each `.version` file individually. If a file contains `1.2.3` (no prefix), it will be updated to `1.3.0`. If it contains `v1.2.3`, it will be updated to `v1.3.0`. This means different `.version` files in the same repository can follow different conventions. Empty `.version` files adopt whatever format the new version tag uses.

### .bumpignore

You can create a `.bumpignore` file in your repository root to exclude directories from the `.version` file scan:

```
# Anchored patterns (match at repo root only)
/vendor
/node_modules

# Unanchored patterns (match at any depth)
testdata
.cache
```

- Lines starting with `/` match directories at the repository root only
- Other patterns match directory names at any depth
- Lines starting with `#` are comments

### .bump.toml — bumping additional files

To bump version strings in files outside the `.version` convention (`Cargo.toml`, `package.json`, Helm charts, custom YAML, etc.), add a `.bump.toml` at the repository root. Each `[[file]]` block points at a single file and either picks a named `format` preset or specifies custom `replace` rules.

```toml
# Use built-in presets for common ecosystems.
[[file]]
path = "Cargo.toml"
format = "cargo"

[[file]]
path = "package.json"
format = "npm"

[[file]]
path = "charts/myapp/Chart.yaml"
format = "chart"

# Or define custom regex replacements. Patterns are multi-line by default,
# so ^ and $ anchor to line boundaries.
[[file]]
path = "deploy.yaml"
replace = [
  { match = '^version: .*$',                template = 'version: {version}' },
  { match = '^(\s*)image: myorg/app:.*$',   template = '${1}image: myorg/app:{tag}' },
]
```

#### Built-in presets

| Preset  | What it rewrites                                                              |
|---------|-------------------------------------------------------------------------------|
| `cargo` | `version = "..."` line in a `Cargo.toml` `[package]` block                    |
| `npm`   | top-level `"version": "..."` in a `package.json`                              |
| `chart` | both `version:` and `appVersion:` lines in a Helm `Chart.yaml`                |

All presets preserve the v-prefix convention of each matched line: `version = "1.0.0"` stays bare, `version = "v1.0.0"` stays prefixed. A file with mixed conventions is handled per match (e.g. `version: 0.5.0` next to `appVersion: "v1.0.0"` keeps each one's prefix).

#### Template variables

Templates support these placeholders, expanded before the regex replacement runs:

- `{auto}` — preserves the v-prefix convention of the matched text (recommended; used by all built-in presets)
- `{version}` — bare semver, always without `v` prefix (e.g. `1.2.3`)
- `{tag}` — semver always with `v` prefix (e.g. `v1.2.3`)
- `{major}`, `{minor}`, `{patch}` — individual components

Regex backreferences (`$1`, `${1}`) pass through to the replacement, so you can capture and preserve indentation as shown in the deploy.yaml example above.

#### Failure modes

- **Zero matches** for any rule is a hard error — the bump is aborted before any commit or tag. This catches typos in patterns that would otherwise silently leave files at the old version.
- **Multiple matches** are all replaced.
- A file may use `format` *or* `replace`, not both.
- Unknown preset names and structurally invalid configs are rejected before any file is written.

