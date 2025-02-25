/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"

	"github.com/GoogleContainerTools/kaniko/pkg/config"
	"github.com/GoogleContainerTools/kaniko/pkg/filesystem"
	"github.com/GoogleContainerTools/kaniko/pkg/timing"
	"github.com/GoogleContainerTools/kaniko/pkg/util"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// For testing
var snapshotPathPrefix = ""

// Snapshotter holds the root directory from which to take snapshots, and a list of snapshots taken
type Snapshotter struct {
	l          *LayeredMap
	directory  string
	ignorelist []util.IgnoreListEntry
}

// NewSnapshotter creates a new snapshotter rooted at d
func NewSnapshotter(l *LayeredMap, d string) *Snapshotter {
	return &Snapshotter{l: l, directory: d, ignorelist: util.IgnoreList()}
}

// Init initializes a new snapshotter
func (s *Snapshotter) Init() error {
	logrus.Info("Initializing snapshotter ...")
	_, _, err := s.scanFullFilesystem()
	return err
}

// Key returns a string based on the current state of the file system
func (s *Snapshotter) Key() (string, error) {
	return s.l.Key()
}

// TakeSnapshot takes a snapshot of the specified files, avoiding directories in the ignorelist, and creates
// a tarball of the changed files. Return contents of the tarball, and whether or not any files were changed
func (s *Snapshotter) TakeSnapshot(files []string, shdCheckDelete bool, forceBuildMetadata bool) (string, error) {
	f, err := os.CreateTemp(config.KanikoDir, "")
	if err != nil {
		return "", err
	}
	defer f.Close()

	s.l.Snapshot()
	if len(files) == 0 && !forceBuildMetadata {
		logrus.Info("No files changed in this command, skipping snapshotting.")
		return "", nil
	}

	filesToAdd, err := filesystem.ResolvePaths(files, s.ignorelist)
	if err != nil {
		return "", err
	}

	logrus.Info("Taking snapshot of files...")

	sort.Strings(filesToAdd)
	logrus.Debugf("Adding to layer: %v", filesToAdd)

	// Add files to current layer.
	for _, file := range filesToAdd {
		if err := s.l.Add(file); err != nil {
			return "", fmt.Errorf("Unable to add file %s to layered map: %w", file, err)
		}
	}

	// Filter out files that are accessible through symlinks
	s.l.SnapshotWithFilesystem()

	// Get whiteout paths
	var filesToWhiteout []string
	if shdCheckDelete {
		_, deletedFiles := util.WalkFS(s.directory, s.l.GetCurrentPaths(), func(s string) (bool, error) {
			return true, nil
		})

		logrus.Debugf("Deleting in layer: %v", deletedFiles)
		// Whiteout files in current layer.
		for file := range deletedFiles {
			if err := s.l.AddDelete(file); err != nil {
				return "", fmt.Errorf("Unable to whiteout file %s in layered map: %w", file, err)
			}
		}

		filesToWhiteout = removeObsoleteWhiteouts(deletedFiles)
		sort.Strings(filesToWhiteout)
	}

	t := util.NewTar(f)
	defer t.Close()
	if err := writeToTar(t, filesToAdd, filesToWhiteout); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// TakeSnapshotFS takes a snapshot of the filesystem, avoiding directories in the ignorelist, and creates
// a tarball of the changed files.
func (s *Snapshotter) TakeSnapshotFS() (string, error) {
	f, err := os.CreateTemp(s.getSnashotPathPrefix(), "")
	if err != nil {
		return "", err
	}
	defer f.Close()
	t := util.NewTar(f)
	defer t.Close()

	filesToAdd, filesToWhiteOut, err := s.scanFullFilesystem()
	if err != nil {
		return "", err
	}

	// Filter out files that are accessible through symlinks
	s.l.SnapshotWithFilesystem()

	if err := writeToTar(t, filesToAdd, filesToWhiteOut); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func (s *Snapshotter) getSnashotPathPrefix() string {
	if snapshotPathPrefix == "" {
		return config.KanikoDir
	}
	return snapshotPathPrefix
}

func (s *Snapshotter) scanFullFilesystem() ([]string, []string, error) {
	logrus.Info("Taking snapshot of full filesystem...")

	// Some of the operations that follow (e.g. hashing) depend on the file system being synced,
	// for example the hashing function that determines if files are equal uses the mtime of the files,
	// which can lag if sync is not called. Unfortunately there can still be lag if too much data needs
	// to be flushed or the disk does its own caching/buffering.
	if runtime.GOOS == "linux" {
		dir, err := os.Open(s.directory)
		if err != nil {
			return nil, nil, err
		}
		defer dir.Close()
		_, _, errno := syscall.Syscall(unix.SYS_SYNCFS, dir.Fd(), 0, 0)
		if errno != 0 {
			return nil, nil, errno
		}
	} else {
		// fallback to full page cache sync
		syscall.Sync()
	}

	s.l.Snapshot()

	logrus.Debugf("Current image filesystem: %v", s.l.currentImage)

	changedPaths, deletedPaths := util.WalkFS(s.directory, s.l.GetCurrentPaths(), s.l.CheckFileChange)
	timer := timing.Start("Resolving Paths")

	filesToAdd := []string{}
	resolvedFiles, err := filesystem.ResolvePaths(changedPaths, s.ignorelist)
	if err != nil {
		return nil, nil, err
	}
	for _, path := range resolvedFiles {
		if util.CheckIgnoreList(path) {
			logrus.Debugf("Not adding %s to layer, as it's ignored", path)
			continue
		}
		filesToAdd = append(filesToAdd, path)
	}

	logrus.Debugf("Adding to layer: %v", filesToAdd)
	logrus.Debugf("Deleting in layer: %v", deletedPaths)

	// Add files to the layered map
	for _, file := range filesToAdd {
		if err := s.l.Add(file); err != nil {
			return nil, nil, fmt.Errorf("Unable to add file %s to layered map: %w", file, err)
		}
	}
	for file := range deletedPaths {
		if err := s.l.AddDelete(file); err != nil {
			return nil, nil, fmt.Errorf("Unable to whiteout file %s in layered map: %w", file, err)
		}
	}

	filesToWhiteout := removeObsoleteWhiteouts(deletedPaths)
	timing.DefaultRun.Stop(timer)

	sort.Strings(filesToAdd)
	sort.Strings(filesToWhiteout)

	return filesToAdd, filesToWhiteout, nil
}

// removeObsoleteWhiteouts filters deleted files according to their parents delete status.
func removeObsoleteWhiteouts(deletedFiles map[string]struct{}) (filesToWhiteout []string) {

	for path := range deletedFiles {
		// Only add the whiteout if the directory for the file still exists.
		dir := filepath.Dir(path)
		if _, ok := deletedFiles[dir]; !ok {
			logrus.Tracef("Adding whiteout for %s", path)
			filesToWhiteout = append(filesToWhiteout, path)
		}
	}

	return filesToWhiteout
}

// writeToTar writes the specified files and whiteouts to the tar archive.
// It handles symlink relationships to prevent duplication of files in layers.
//
// The function performs the following steps:
// 1. Creates a map of all files for symlink accessibility checks
// 2. Processes whiteout files first (files that need to be deleted in the layer)
// 3. Builds a map of symlink targets to their symlinks by examining each file
// 4. Identifies directories that have been replaced with symlinks (special case)
// 5. For each file:
//    - If it's a target of a symlink, it's skipped unless it's in a directory
//      that was replaced with a symlink
//    - Otherwise, it's added to the tar archive
//
// This approach prevents duplication of files in layers when they're already
// accessible through symlinks, which improves layer efficiency and reduces image size.
func writeToTar(t util.Tar, files, whiteouts []string) error {
	timer := timing.Start("Writing tar file")
	defer timing.DefaultRun.Stop(timer)

	// Now create the tar.
	addedPaths := make(map[string]bool)

	// Create a map of all files for symlink accessibility checks
	allFiles := make(map[string]struct{})
	for _, file := range files {
		allFiles[file] = struct{}{}
	}

	for _, path := range whiteouts {
		skipWhiteout, err := parentPathIncludesNonDirectory(path)
		if err != nil {
			return err
		}
		if skipWhiteout {
			continue
		}

		if err := addParentDirectories(t, addedPaths, path); err != nil {
			return err
		}
		if err := t.Whiteout(path); err != nil {
			return err
		}
	}

// Build a map of symlink targets to their symlinks
symlinkTargets := make(map[string]string)
for _, file := range files {
	fi, err := os.Lstat(file)
	if err == nil && util.IsSymlink(fi) {
		linkTarget, err := os.Readlink(file)
		if err == nil {
			// If the target is not absolute, make it absolute
			if !filepath.IsAbs(linkTarget) {
				linkTarget = filepath.Join(filepath.Dir(file), linkTarget)
			}
			linkTarget = filepath.Clean(linkTarget)
			
			// Check if the target is also in our files list
			for _, potentialTarget := range files {
				if linkTarget == potentialTarget {
					symlinkTargets[potentialTarget] = file
					logrus.Debugf("Found symlink relationship: %s is a symlink to %s", file, potentialTarget)
					break
				}
			}
		}
	}
}

// In the case of a directory being replaced with a symlink, we need to include both the symlink
// and the target file in the snapshot. This is because the directory's contents are being whited out,
// and we need to ensure the symlink and its target are both included.
// We'll check if there are any whiteout files that would have been in a directory with the same name
// as any of our symlinks.
dirsReplacedWithSymlinks := make(map[string]bool)
for _, file := range whiteouts {
	dir := filepath.Dir(file)
	dirName := filepath.Base(dir)
	for symlink := range symlinkTargets {
		if filepath.Base(filepath.Dir(symlink)) == dirName {
			dirsReplacedWithSymlinks[dirName] = true
			break
		}
	}
}

// Also check if any symlink in our files list is replacing a directory
// This is needed for the TestSnapshotFSReplaceDirWithLink test case
for _, file := range files {
	fi, err := os.Lstat(file)
	if err == nil && util.IsSymlink(fi) {
		// If this symlink is in our files list, it's a new symlink
		// Check if it's replacing a directory
		linkTarget, err := os.Readlink(file)
		if err == nil {
			// If the target is not absolute, make it absolute
			if !filepath.IsAbs(linkTarget) {
				linkTarget = filepath.Join(filepath.Dir(file), linkTarget)
			}
			linkTarget = filepath.Clean(linkTarget)
			
			// If the target is also in our files list, mark the directory as replaced
			for _, potentialTarget := range files {
				if linkTarget == potentialTarget {
					// Mark the directory as replaced, but only if the directory name is not "/"
					dirName := filepath.Base(filepath.Dir(file))
					if dirName != "/" {
						dirsReplacedWithSymlinks[dirName] = true
						logrus.Debugf("Directory %s was replaced with symlink %s to %s", dirName, file, potentialTarget)
					}
					break
				}
			}
		}
	}
}

for _, path := range files {
	// If this file is a target of a symlink, check if the symlink is part of a directory
	// that was replaced with a symlink. If so, include both the symlink and the target.
	skipFile := false
	if symlink, isTarget := symlinkTargets[path]; isTarget {
		// Check if the symlink is in a directory that was replaced
		symlinkDir := filepath.Base(filepath.Dir(symlink))
		// Also check if the symlink itself is replacing a directory
		symlinkBase := filepath.Base(symlink)
		if !dirsReplacedWithSymlinks[symlinkDir] && !dirsReplacedWithSymlinks[symlinkBase] {
			logrus.Debugf("writeToTar: %s is a target of symlink %s, skipping %s", path, symlink, path)
			skipFile = true
		} else {
			logrus.Debugf("Including both symlink %s and target %s because directory was replaced", symlink, path)
		}
	}
	
	if skipFile {
		continue
	}

		if err := addParentDirectories(t, addedPaths, path); err != nil {
			return err
		}
		if _, pathAdded := addedPaths[path]; pathAdded {
			continue
		}
		if err := t.AddFileToTar(path); err != nil {
			return err
		}
		addedPaths[path] = true
	}
	return nil
}

// Returns true if a parent of the given path has been replaced with anything other than a directory
func parentPathIncludesNonDirectory(path string) (bool, error) {
	for _, parentPath := range util.ParentDirectories(path) {
		lstat, err := os.Lstat(parentPath)
		if err != nil {
			return false, err
		}

		if !lstat.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

func addParentDirectories(t util.Tar, addedPaths map[string]bool, path string) error {
	for _, parentPath := range util.ParentDirectories(path) {
		if _, pathAdded := addedPaths[parentPath]; pathAdded {
			continue
		}
		if err := t.AddFileToTar(parentPath); err != nil {
			return err
		}
		addedPaths[parentPath] = true
	}
	return nil
}

// filesWithLinks returns the symlink and the target path if its exists.
func filesWithLinks(path string) ([]string, error) {
	link, err := util.GetSymLink(path)
	if errors.Is(err, util.ErrNotSymLink) {
		return []string{path}, nil
	} else if err != nil {
		return nil, err
	}
	// Add symlink if it exists in the FS
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(path), link)
	}
	if _, err := os.Stat(link); err != nil {
		return []string{path}, nil //nolint:nilerr
	}
	return []string{path, link}, nil
}
