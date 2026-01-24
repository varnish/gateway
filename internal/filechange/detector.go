// Package filechange provides utilities for detecting actual file changes,
// handling cases like Kubernetes ConfigMap atomic swaps where directory events
// may fire without the target file actually changing.
package filechange

import (
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

// Detector tracks file metadata to detect actual changes.
// It compares both modification time and inode to handle ConfigMap atomic swaps
// where symlinks are updated but the underlying file may not change.
type Detector struct {
	mu        sync.Mutex
	lastMtime time.Time
	lastInode uint64
}

// HasChanged checks if the file at path has changed since the last check.
// Returns true if the file has changed (mtime or inode different).
// Returns false if unchanged or if the file cannot be stat'd (which may happen
// temporarily during ConfigMap updates).
//
// This method is safe for concurrent use.
func (d *Detector) HasChanged(path string, logger *slog.Logger) bool {
	info, err := os.Stat(path)
	if err != nil {
		// File might be temporarily missing during ConfigMap update
		logger.Debug("file stat failed, skipping", "path", path, "error", err)
		return false
	}

	// Get inode for comparison (Linux-specific)
	var inode uint64
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		inode = stat.Ino
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Check if file hasn't actually changed
	unchanged := d.lastMtime.Equal(info.ModTime()) && d.lastInode == inode
	if unchanged {
		return false
	}

	// File has changed, update tracking info
	d.lastMtime = info.ModTime()
	d.lastInode = inode
	return true
}

// GetMetadata returns the current tracked metadata for logging/debugging.
// Returns (mtime, inode). Safe for concurrent use.
func (d *Detector) GetMetadata() (time.Time, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastMtime, d.lastInode
}
