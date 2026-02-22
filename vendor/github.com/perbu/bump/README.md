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

