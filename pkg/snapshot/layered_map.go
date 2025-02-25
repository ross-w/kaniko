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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleContainerTools/kaniko/pkg/timing"
	"github.com/GoogleContainerTools/kaniko/pkg/util"
	"github.com/sirupsen/logrus"
)

type LayeredMap struct {
	adds    []map[string]string   // All layers with added files.
	deletes []map[string]struct{} // All layers with deleted files.

	currentImage        map[string]string // All files and hashes in the current image (up to the last layer).
	isCurrentImageValid bool              // If the currentImage is not out-of-date.

	layerHashCache map[string]string
	hasher         func(string) (string, error)
}

// NewLayeredMap creates a new layered map which keeps track of adds and deletes.
func NewLayeredMap(h func(string) (string, error)) *LayeredMap {
	l := LayeredMap{
		hasher: h,
	}

	l.currentImage = map[string]string{}
	l.layerHashCache = map[string]string{}
	return &l
}

// Snapshot creates a new layer.
func (l *LayeredMap) Snapshot() {
	// Save current state of image
	l.updateCurrentImage()

	l.adds = append(l.adds, map[string]string{})
	l.deletes = append(l.deletes, map[string]struct{}{})
	l.layerHashCache = map[string]string{} // Erase the hash cache for this new layer.
}

// SnapshotWithFilesystem creates a new layer and filters out files that are accessible through symlinks.
// This is used by the executor after all files have been added to the layer to prevent duplication
// of files that are already accessible through symlinks.
//
// The function performs the following steps:
// 1. Identifies symlink relationships between files in the current layer by scanning for symlinks
//    and determining their targets
// 2. Detects directories that have been replaced with symlinks (a special case that requires
//    both the symlink and its target to be included in the layer)
// 3. Filters out files that are targets of symlinks (unless they're in a directory that was
//    replaced with a symlink)
// 4. Filters out files that are accessible through symlinks using IsAccessibleThroughSymlink
//
// This ensures that files are not duplicated in layers when they're already accessible through symlinks,
// which improves layer efficiency and reduces image size.
//
// For example, if file B is a symlink to file A, then file A is accessible through a symlink and doesn't
// need to be duplicated in the layer. However, if file B is in a directory that was replaced with a symlink,
// then both file A and file B need to be included in the layer.
func (l *LayeredMap) SnapshotWithFilesystem() {
	// Save current state of image
	l.updateCurrentImage()

	// Create a new layer
	newAdds := map[string]string{}
	newDeletes := map[string]struct{}{}

	// Get all files in the current layer
	currentAdds := l.adds[len(l.adds)-1]
	currentDeletes := l.deletes[len(l.deletes)-1]

	// Create a map of all files in the current layer for symlink accessibility checks
	allFiles := make(map[string]struct{})
	for file := range currentAdds {
		allFiles[file] = struct{}{}
	}

	// Build a map of symlink targets to their symlinks
	symlinkTargets := make(map[string]string)
	for file := range currentAdds {
		fi, err := os.Lstat(file)
		if err == nil && util.IsSymlink(fi) {
			linkTarget, err := os.Readlink(file)
			if err == nil {
				// If the target is not absolute, make it absolute
				if !filepath.IsAbs(linkTarget) {
					linkTarget = filepath.Join(filepath.Dir(file), linkTarget)
				}
				linkTarget = filepath.Clean(linkTarget)
				
				// Check if the target is also in our current adds
				if _, exists := currentAdds[linkTarget]; exists {
					symlinkTargets[linkTarget] = file
					logrus.Debugf("Found symlink relationship: %s is a symlink to %s", file, linkTarget)
				}
			}
		}
	}

	// Check for directories replaced with symlinks
	dirsReplacedWithSymlinks := make(map[string]bool)
	for file := range currentAdds {
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
				
				// If the target is also in our current adds, mark the directory as replaced
				if _, exists := currentAdds[linkTarget]; exists {
					// Mark the directory as replaced, but only if the directory name is not "/"
					dirName := filepath.Base(filepath.Dir(file))
					if dirName != "/" {
						dirsReplacedWithSymlinks[dirName] = true
						logrus.Debugf("Directory %s was replaced with symlink %s to %s", dirName, file, linkTarget)
					}
				}
			}
		}
	}

	// Filter out files that are targets of symlinks or accessible through symlinks
	for file, hash := range currentAdds {
		if symlink, isTarget := symlinkTargets[file]; isTarget {
			// Check if the symlink is in a directory that was replaced
			symlinkDir := filepath.Base(filepath.Dir(symlink))
			// Also check if the symlink itself is replacing a directory
			symlinkBase := filepath.Base(symlink)
			if !dirsReplacedWithSymlinks[symlinkDir] && !dirsReplacedWithSymlinks[symlinkBase] {
				logrus.Debugf("SnapshotWithFilesystem: %s is a target of symlink %s, removing from layer", file, symlink)
				continue
			} else {
				logrus.Debugf("Including both symlink %s and target %s because directory was replaced", symlink, file)
				newAdds[file] = hash
			}
		} else if !util.IsAccessibleThroughSymlink(file, allFiles) {
			newAdds[file] = hash
		} else {
			logrus.Debugf("File %s is accessible through a symlink, not adding to layer", file)
		}
	}

	// Copy over the deletes
	for file := range currentDeletes {
		newDeletes[file] = struct{}{}
	}

	// Replace the current layer with the filtered layer
	l.adds[len(l.adds)-1] = newAdds
	l.deletes[len(l.deletes)-1] = newDeletes
	l.isCurrentImageValid = false
}

// Key returns a hash for added and delted files.
func (l *LayeredMap) Key() (string, error) {

	var adds map[string]string
	var deletes map[string]struct{}

	if len(l.adds) != 0 {
		adds = l.adds[len(l.adds)-1]
		deletes = l.deletes[len(l.deletes)-1]
	}

	c := bytes.NewBuffer([]byte{})
	enc := json.NewEncoder(c)
	err := enc.Encode(adds)
	if err != nil {
		return "", err
	}
	err = enc.Encode(deletes)
	if err != nil {
		return "", err
	}
	return util.SHA256(c)
}

// getCurrentImage returns the current image by merging the latest
// adds and deletes on to the current image (if its not yet valid.)
func (l *LayeredMap) getCurrentImage() map[string]string {
	if l.isCurrentImageValid || len(l.adds) == 0 {
		// No layers yet or current image is valid.
		return l.currentImage
	}

	current := map[string]string{}

	// Copy current image paths/hashes.
	for p, h := range l.currentImage {
		current[p] = h
	}

	// Add the last layer on top.
	addedFiles := l.adds[len(l.adds)-1]
	deletedFiles := l.deletes[len(l.deletes)-1]

	for add, hash := range addedFiles {
		current[add] = hash
	}

	for del := range deletedFiles {
		delete(current, del)
	}

	return current
}

// updateCurrentImage update the internal current image by merging the
// top adds and deletes onto the current image.
func (l *LayeredMap) updateCurrentImage() {
	if l.isCurrentImageValid {
		return
	}

	l.currentImage = l.getCurrentImage()
	l.isCurrentImageValid = true
}

// get returns the current hash in the current image `l.currentImage`.
func (l *LayeredMap) get(s string) (string, bool) {
	h, ok := l.currentImage[s]
	return h, ok
}

// GetCurrentPaths returns all existing paths in the actual current image
// cached by FlattenLayers.
func (l *LayeredMap) GetCurrentPaths() map[string]struct{} {
	current := l.getCurrentImage()

	paths := map[string]struct{}{}
	for f := range current {
		paths[f] = struct{}{}
	}
	return paths
}

// AddDelete will delete the specific files in the current layer.
func (l *LayeredMap) AddDelete(s string) error {
	l.isCurrentImageValid = false

	l.deletes[len(l.deletes)-1][s] = struct{}{}
	return nil
}

// Add will add the specified file s to the current layer.
func (l *LayeredMap) Add(s string) error {
	l.isCurrentImageValid = false

	// Check if the file is accessible through a symlink first
	// If it is, don't add it to the layer to prevent duplication
	if l.isAccessibleThroughSymlink(s) {
		logrus.Debugf("File %s is accessible through a symlink, not adding to layer", s)
		return nil
	}

	// Use hash function and add to layers
	newV, err := func(s string) (string, error) {
		if v, ok := l.layerHashCache[s]; ok {
			return v, nil
		}
		return l.hasher(s)
	}(s)

	if err != nil {
		return fmt.Errorf("Error creating hash for %s: %w", s, err)
	}

	l.adds[len(l.adds)-1][s] = newV
	return nil
}

// isAccessibleThroughSymlink checks if a file is accessible through any symlink in the current image.
// This is used to prevent duplication of files in layers when they're already accessible through symlinks.
// 
// The function works by:
// 1. Getting all files in the current image
// 2. Delegating to the util.IsAccessibleThroughSymlink function which:
//    - Resolves the absolute path of the file being checked
//    - Examines each file in the provided paths to see if it's a symlink
//    - For each symlink, resolves its target and compares with the file being checked
//
// For example, if file B is a symlink to file A, then file A is accessible through a symlink and doesn't
// need to be duplicated in the layer. This improves layer efficiency and reduces image size.
//
// Returns true if the file is accessible through a symlink, false otherwise.
func (l *LayeredMap) isAccessibleThroughSymlink(path string) bool {
	// Get all files in the current image
	currentPaths := l.GetCurrentPaths()
	
	// Use the utility function to check if the file is accessible through a symlink
	return util.IsAccessibleThroughSymlink(path, currentPaths)
}

// CheckFileChange checks whether a given file (needs to exist) changed
// from the current layered map by its hashing function.
// If the file does not exist, an error is returned.
// Returns true if the file is changed.
func (l *LayeredMap) CheckFileChange(s string) (bool, error) {
	t := timing.Start("Hashing files")
	defer timing.DefaultRun.Stop(t)

	// Check if the file is accessible through a symlink first
	// If it is, consider it unchanged to prevent duplication
	if l.isAccessibleThroughSymlink(s) {
		logrus.Debugf("File %s is accessible through a symlink, considering it unchanged", s)
		return false, nil
	}

	newV, err := l.hasher(s)
	if err != nil {
		return false, err
	}

	// Save hash to not recompute it when
	// adding the file.
	l.layerHashCache[s] = newV

	oldV, ok := l.get(s)
	if ok && newV == oldV {
		// File hash did not change => Unchanged.
		return false, nil
	}

	// File does not exist in current image,
	// or it did change => Changed.
	return true, nil
}
