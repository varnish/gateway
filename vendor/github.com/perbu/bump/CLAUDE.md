# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Go-based command-line tool called "bump" that manages semantic version bumping in git repositories. It serves as a replacement for the deprecated standard-version tool.

## Key Architecture

- **Single-file application**: The entire application is contained in `main.go` (313 lines)
- **Git integration**: Uses `go-git/go-git/v5` library for git operations (tags, commits, worktree)
- **Semver handling**: Uses `golang.org/x/mod/semver` for semantic version parsing and sorting
- **Version tracking**: Looks for `.version` files throughout the repository to update version numbers
- **Embedded version**: The tool's own version is embedded from `.version` file using `//go:embed`

## Core Workflow

1. Reads all git tags and finds the highest semantic version
2. Increments version based on command-line flags (patch/minor/major)
3. **Validates that target tag doesn't already exist** (prevents partial commits)
4. Updates all `.version` files in the repository with new version
5. Commits changes with version bump message
6. Creates new git tag with the bumped version

## Development Commands

```bash
# Build and test the application
go build -o /dev/null .

# Run tests
go test -v

# Run specific test
go test -v -run TestName

# Run the application
go run main.go [flags]

# Install from source
go install github.com/perbu/bump@latest
```

## Command Line Interface

- `-patch`: Increment patch version (default behavior)
- `-minor`: Increment minor version  
- `-major`: Increment major version
- `-version string`: Set specific initial version
- `-dry-run`: Preview changes without writing to repository
- `-force`: Override dirty repository check
- `-help`: Show usage information

## Important Constraints

- Repository must be clean (unless `-force` is used)
- Requires existing version tags in git to determine current version
- All `.version` files must contain valid semver or be empty
- Uses SSH agent for commit signing when available
- Tag existence is validated before making any commits (atomic operation)

## Testing

The codebase includes tests in `main_test.go`:
- **TestBumpWhenTagAlreadyExists**: Verifies that no commits are created when target tag already exists
- **TestBumpNormalOperation**: Ensures normal version bumping works correctly

Key functions:
- `tagExists()`: Helper function to check if a tag already exists (main.go:142)
- `run()`: Main logic with tag validation before commits (main.go:49)