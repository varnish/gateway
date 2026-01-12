package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/mod/semver"
)

//go:embed .version
var embeddedVersion string

type action int

const (
	noAction action = iota
	incrementPatch
	incrementMinor
	incrementMajor
)

type config struct {
	version string
	action  action
	dryRun  bool
	forced  bool
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	err := run(ctx, os.Stdout, os.Args[1:], os.Environ())
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, output io.Writer, argv []string, env []string) error {
	_, _ = fmt.Fprintf(output, "bump %s bumping\n", embeddedVersion)
	runConfig, showHelp, err := getConfig(argv)
	if err != nil {
		return fmt.Errorf("getConfig: %w", err)
	}
	if showHelp {
		return nil
	}

	repo, err := git.PlainOpen(".")
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	// check that the repository is clean
	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("repo.Worktree: %w", err)
	}
	status, err := w.Status()
	if err != nil {
		return fmt.Errorf("worktree.Status: %w", err)
	}

	// Filter out files that shouldn't block bumping
	// go-git's Status() can report files that native git doesn't consider dirty:
	// - Ignored files (untracked in both worktree and staging)
	// - Files with only metadata changes (permissions) when filemode=false
	// - Line ending differences when autocrlf is configured
	//
	// The approach: if a file is Modified in worktree but Unmodified in staging,
	// it's likely a go-git quirk. We trust native git's behavior over go-git.
	cleanStatus := make(git.Status)
	for file, fileStatus := range status {
		// Skip untracked files (includes ignored files)
		if fileStatus.Worktree == git.Untracked && fileStatus.Staging == git.Untracked {
			continue
		}
		// Skip files that show as Modified/Unmodified - this is a go-git quirk
		// where it detects changes that git itself doesn't consider dirty
		// (e.g., filemode, line endings with autocrlf, etc.)
		if fileStatus.Worktree == git.Modified && fileStatus.Staging == git.Unmodified {
			continue
		}
		cleanStatus[file] = fileStatus
	}

	if !cleanStatus.IsClean() && !runConfig.forced {
		// Provide detailed information about what's dirty
		var reasons []string
		for file, fileStatus := range cleanStatus {
			reasons = append(reasons, fmt.Sprintf("  %s: worktree=%v staging=%v", file, fileStatus.Worktree, fileStatus.Staging))
		}
		if len(reasons) > 0 {
			return fmt.Errorf("repository is not clean (use -force to override):\n%s", strings.Join(reasons, "\n"))
		}
		return fmt.Errorf("repository is not clean (use -force to override)")
	}

	if runConfig.version != "" {
		// Normalize version for validation (semver requires "v" prefix)
		normalizedVersion := normalizeVersion(runConfig.version)
		if !semver.IsValid(normalizedVersion) {
			return fmt.Errorf("invalid semantic version string: '%s'", runConfig.version)
		}

		// Check if tag already exists before making any changes
		exists, err := tagExists(repo, runConfig.version)
		if err != nil {
			return fmt.Errorf("failed to check if tag exists: %w", err)
		}
		if exists {
			return fmt.Errorf("tag '%s' already exists", runConfig.version)
		}

		err = updateVersionFiles(repo, runConfig, output, runConfig.version)
		if err != nil {
			return fmt.Errorf("updateVersionFiles: %w", err)
		}
		hash, err := tagVersion(repo, runConfig, runConfig.version)
		if err != nil {
			return fmt.Errorf("tagVersion: %w", err)
		}
		_, _ = fmt.Fprintf(output, "Set version %s, tag=%s\n", runConfig.version, hash)
		return nil
	}
	// increment version
	currentVersion, err := lastTag(repo)
	if err != nil {
		return fmt.Errorf("failed to get last tag: %w", err)
	}

	// Check if there are changes since the last tag
	hasChanges, err := hasChangesSinceTag(repo, currentVersion)
	if err != nil {
		return fmt.Errorf("failed to check for changes since last tag: %w", err)
	}
	if !hasChanges && !runConfig.forced {
		return fmt.Errorf("no changes since last version tag '%s' (use -force to override)", currentVersion)
	}

	newVersion, err := incrementVersion(currentVersion, runConfig)
	if err != nil {
		return fmt.Errorf("incrementVersion: %w", err)
	}

	// Check if the target tag already exists before making any changes
	exists, err := tagExists(repo, newVersion)
	if err != nil {
		return fmt.Errorf("failed to check if tag exists: %w", err)
	}
	if exists {
		return fmt.Errorf("tag '%s' already exists", newVersion)
	}

	err = updateVersionFiles(repo, runConfig, output, newVersion)
	if err != nil {
		return fmt.Errorf("updateVersionFiles: %w", err)
	}
	tag, err := tagVersion(repo, runConfig, newVersion)
	if err != nil {
		return fmt.Errorf("tagVersion: %w", err)
	}
	_, _ = fmt.Fprintf(output, "Bumped version %s --> %s, tag=%s\n", currentVersion,
		newVersion, tag)
	return nil
}

func lastTag(repo *git.Repository) (string, error) {
	// Get the list of tags
	tagRefs, err := repo.Tags()
	if err != nil {
		return "", fmt.Errorf("failed to get tags: %w", err)
	}
	var tags []string
	// Map to track original format for each normalized tag
	originalFormat := make(map[string]string)
	err = tagRefs.ForEach(func(t *plumbing.Reference) error {
		tagName := t.Name().Short()
		// Normalize for validation (semver requires "v" prefix)
		normalizedTag := normalizeVersion(tagName)
		// check that the tag matches the semver format
		if !semver.IsValid(normalizedTag) {
			return nil
		}
		tags = append(tags, normalizedTag)
		// Store original format (prefer the one without "v" if we encounter duplicates)
		if _, exists := originalFormat[normalizedTag]; !exists || !hasVPrefix(tagName) {
			originalFormat[normalizedTag] = tagName
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to iterate over tags: %w", err)
	}
	if len(tags) == 0 {
		return "", errors.New("no version tags found in the repository")
	}
	// sort the normalized tags
	semver.Sort(tags)
	// return the highest tag in its original format
	highestNormalized := tags[len(tags)-1]
	return originalFormat[highestNormalized], nil
}

func tagExists(repo *git.Repository, tagName string) (bool, error) {
	tagRefs, err := repo.Tags()
	if err != nil {
		return false, fmt.Errorf("failed to get tags: %w", err)
	}

	exists := false
	err = tagRefs.ForEach(func(t *plumbing.Reference) error {
		if t.Name().Short() == tagName {
			exists = true
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to iterate over tags: %w", err)
	}

	return exists, nil
}

// hasChangesSinceTag checks if there are any commits since the given tag
func hasChangesSinceTag(repo *git.Repository, tagName string) (bool, error) {
	// Get all tags and find the one we're looking for
	tagRefs, err := repo.Tags()
	if err != nil {
		return false, fmt.Errorf("failed to get tags: %w", err)
	}

	var tagHash plumbing.Hash
	found := false
	err = tagRefs.ForEach(func(t *plumbing.Reference) error {
		if t.Name().Short() == tagName {
			tagHash = t.Hash()
			found = true
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to iterate tags: %w", err)
	}
	if !found {
		return false, fmt.Errorf("tag not found: %s", tagName)
	}

	// Try to get as commit object first (for lightweight tags)
	commit, err := repo.CommitObject(tagHash)
	if err != nil {
		// If that fails, it might be an annotated tag
		tagObj, err := repo.TagObject(tagHash)
		if err != nil {
			return false, fmt.Errorf("failed to get tag or commit object: %w", err)
		}
		// Get the commit the tag points to
		commit, err = repo.CommitObject(tagObj.Target)
		if err != nil {
			return false, fmt.Errorf("failed to get commit from tag: %w", err)
		}
	}

	// Get the current HEAD
	head, err := repo.Head()
	if err != nil {
		return false, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// If HEAD is the same as the tag commit, there are no changes
	return head.Hash() != commit.Hash, nil
}

// hasVPrefix checks if a version string starts with "v"
func hasVPrefix(version string) bool {
	return len(version) > 0 && version[0] == 'v'
}

// normalizeVersion ensures a version has "v" prefix for semver operations
func normalizeVersion(version string) string {
	if hasVPrefix(version) {
		return version
	}
	return "v" + version
}

// stripVPrefix removes "v" prefix if present
func stripVPrefix(version string) string {
	if hasVPrefix(version) {
		return version[1:]
	}
	return version
}

func getConfig(args []string) (config, bool, error) {
	var cfg config
	var showhelp, patchFlag, minorFlag, majorFlag bool

	flagSet := flag.NewFlagSet("version", flag.ContinueOnError)
	flagSet.StringVar(&cfg.version, "version", "", "Initial version number.")
	flagSet.BoolVar(&patchFlag, "patch", false, "Increase patch version.")
	flagSet.BoolVar(&minorFlag, "minor", false, "Increase minor version.")
	flagSet.BoolVar(&majorFlag, "major", false, "Increase major version.")
	flagSet.BoolVar(&cfg.dryRun, "dry-run", false, "Do not write changes to the repository.")
	flagSet.BoolVar(&cfg.forced, "force", false, "Force the action despite the repository being dirty.")
	flagSet.BoolVar(&showhelp, "help", false, "Show help message.")

	err := flagSet.Parse(args)
	if err != nil {
		return config{}, false, fmt.Errorf("failed to parse flags: %w", err)
	}
	if showhelp {
		flagSet.Usage()
		return config{}, true, nil
	}
	// check if there are any arguments left
	if flagSet.NArg() > 0 {
		return config{}, false, fmt.Errorf("unexpected arguments: %s", flagSet.Args())
	}

	// if both version and increment flags are set, return an error
	if cfg.version != "" && (patchFlag || minorFlag || majorFlag) {
		return config{}, false, fmt.Errorf("cannot set version and increment flags at the same time")
	}
	// check that not more than one flag is set:
	if (patchFlag && minorFlag) || (patchFlag && majorFlag) || (minorFlag && majorFlag) {
		return config{}, false, fmt.Errorf("cannot set more than one increment flag at the same time")
	}
	if patchFlag {
		cfg.action = incrementPatch
	}
	if minorFlag {
		cfg.action = incrementMinor
	}
	if majorFlag {
		cfg.action = incrementMajor
	}
	// no action not version given: increment patch
	if cfg.action == noAction && cfg.version == "" {
		cfg.action = incrementPatch
	}
	return cfg, false, nil
}

func updateVersionFiles(repo *git.Repository, cfg config, output io.Writer, newVersion string) error {
	// Track if any files were updated
	filesUpdated := 0

	// find all the files name ".version"
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("failed to walk directory: %w", err)
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "vendor" || d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.Name() != ".version" {
			return nil
		}
		// read the content of the file
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file: %w", err)
		}
		// content must either by empty or a valid semver, if not we return an error
		trimmedContent := strings.TrimSpace(string(content))
		if len(trimmedContent) > 0 && !semver.IsValid(normalizeVersion(trimmedContent)) {
			return fmt.Errorf("invalid version in file %s: '%s'", path, trimmedContent)
		}
		// print the action to the output.
		_, _ = fmt.Fprintf(output, "Updating version in file %s to %s\n", path, newVersion)

		filesUpdated++

		if cfg.dryRun {
			return nil // return early if we are in dry-run mode
		}
		// write the new version to the file
		err = os.WriteFile(path, []byte(newVersion), 0644)
		if err != nil {
			return fmt.Errorf("failed to write file: %w", err)
		}
		// add the file to the repository
		err = add(repo, path)
		if err != nil {
			return fmt.Errorf("failed to add file: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk directory: %w", err)
	}

	// Only commit if not in dry-run mode and files were actually updated
	if !cfg.dryRun && filesUpdated > 0 {
		// commit the changes
		err = commit(repo, fmt.Sprintf("bump version to %s", newVersion))
		if err != nil {
			return fmt.Errorf("commit: %w", err)
		}
	}
	return nil
}

func incrementVersion(currentVersion string, cfg config) (string, error) {
	// Detect if the current version uses "v" prefix
	useVPrefix := hasVPrefix(currentVersion)

	// Strip "v" prefix for parsing
	versionToParse := stripVPrefix(currentVersion)

	parts := strings.Split(versionToParse, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid version format: %s", currentVersion)
	}

	var major, minor, patch int
	_, err := fmt.Sscanf(versionToParse, "%d.%d.%d", &major, &minor, &patch)
	if err != nil {
		return "", fmt.Errorf("failed to parse current version('%s'): %w", currentVersion, err)
	}
	switch cfg.action {
	case incrementPatch:
		patch++
	case incrementMinor:
		minor++
		patch = 0
	case incrementMajor:
		major++
		minor = 0
		patch = 0
	default:
		return "", fmt.Errorf("invalid action: %d", cfg.action)
	}

	// Return version in the same format as input
	if useVPrefix {
		return fmt.Sprintf("v%d.%d.%d", major, minor, patch), nil
	}
	return fmt.Sprintf("%d.%d.%d", major, minor, patch), nil
}

func tagVersion(repo *git.Repository, cfg config, version string) (string, error) {
	// find the current commit
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD: %w", err)
	}
	opts := &git.CreateTagOptions{
		Message: "tag created by bump",
	}
	if cfg.dryRun {
		return head.Hash().String(), nil
	}
	ref, err := repo.CreateTag(version, head.Hash(), opts)
	if err != nil {
		return "", fmt.Errorf("failed to create tag: %w", err)
	}
	return ref.Hash().String(), nil
}

// add adds the file at the given path to the repository
func add(repo *git.Repository, path string) error {
	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("repo.Worktree: %w", err)
	}
	_, err = w.Add(path)
	if err != nil {
		return fmt.Errorf("worktree.Add(%s): %w", path, err)
	}
	return nil
}

func commit(repo *git.Repository, message string) error {
	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("repo.Worktree: %w", err)
	}
	_, err = w.Commit(message, &git.CommitOptions{})
	if err != nil {
		return fmt.Errorf("worktree.Commit: %w", err)
	}
	return nil
}
