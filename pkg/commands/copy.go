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

package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kConfig "github.com/GoogleContainerTools/kaniko/pkg/config"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/GoogleContainerTools/kaniko/pkg/dockerfile"
	"github.com/GoogleContainerTools/kaniko/pkg/util"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// for testing
var (
	getUserGroup = util.GetUserGroup
)

type CopyCommand struct {
	BaseCommand
	cmd           *instructions.CopyCommand
	fileContext   util.FileContext
	snapshotFiles []string
	shdCache      bool
}

func (c *CopyCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	// Resolve from
	if c.cmd.From != "" {
		c.fileContext = util.FileContext{Root: filepath.Join(kConfig.KanikoDir, c.cmd.From)}
	}

	replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
	uid, gid, err := getUserGroup(c.cmd.Chown, replacementEnvs)
	logrus.Debugf("found uid %v and gid %v for chown string %v", uid, gid, c.cmd.Chown)
	if err != nil {
		return errors.Wrap(err, "getting user group from chown")
	}

	// sources from the Copy command are resolved with wildcards {*?[}
	srcs, dest, err := util.ResolveEnvAndWildcards(c.cmd.SourcesAndDest, c.fileContext, replacementEnvs)
	if err != nil {
		return errors.Wrap(err, "resolving src")
	}

	chmod, useDefaultChmod, err := util.GetChmod(c.cmd.Chmod, replacementEnvs)
	if err != nil {
		return errors.Wrap(err, "getting permissions from chmod")
	}

	// For each source, iterate through and copy it over
	for _, src := range srcs {
		fullPath := filepath.Join(c.fileContext.Root, src)

		fi, err := os.Lstat(fullPath)
		if err != nil {
			return errors.Wrap(err, "could not copy source")
		}
		if fi.IsDir() && !strings.HasSuffix(fullPath, string(os.PathSeparator)) {
			fullPath += "/"
		}
		cwd := config.WorkingDir
		if cwd == "" {
			cwd = kConfig.RootDir
		}

		destPath, err := util.DestinationFilepath(fullPath, dest, cwd)
		if err != nil {
			return errors.Wrap(err, "find destination path")
		}

		// If the destination dir is a symlink we need to resolve the path and use
		// that instead of the symlink path
		destPath, err = resolveIfSymlink(destPath)
		if err != nil {
			return errors.Wrap(err, "resolving dest symlink")
		}

		// We don't need to check if the destination is accessible through a symlink
		// when copying files. The symlink accessibility check is only needed when
		// deciding which files to include in a layer, which is handled in FilesToSnapshot.
		// The resolveIfSymlink function above already handles resolving the symlink path.
		
		// Note: Previously, we were checking if the destination was accessible through a symlink
		// and skipping it if it was. This caused issues when copying to a directory that was
		// itself a symlink, as we would skip copying all files.

		if fi.IsDir() {
			copiedFiles, err := util.CopyDir(fullPath, destPath, c.fileContext, uid, gid, chmod, useDefaultChmod)
			if err != nil {
				return errors.Wrap(err, "copying dir")
			}
			c.snapshotFiles = append(c.snapshotFiles, copiedFiles...)
		} else if util.IsSymlink(fi) {
			// If file is a symlink, preserve it by creating a new symlink
			exclude, err := util.CopySymlink(fullPath, destPath, c.fileContext)
			if err != nil {
				return errors.Wrap(err, "copying symlink")
			}
			if exclude {
				continue
			}
			c.snapshotFiles = append(c.snapshotFiles, destPath)
		} else {
			// ... Else, we want to copy over a file
			exclude, err := util.CopyFile(fullPath, destPath, c.fileContext, uid, gid, chmod, useDefaultChmod)
			if err != nil {
				return errors.Wrap(err, "copying file")
			}
			if exclude {
				continue
			}
			c.snapshotFiles = append(c.snapshotFiles, destPath)
		}
	}
	return nil
}

// FilesToSnapshot returns a list of files that should be included in the snapshot.
// It filters out files that are accessible through symlinks to prevent duplication in layers.
// 
// The function performs the following steps:
// 1. Creates a map of all files in the snapshot for symlink accessibility checks
// 2. Identifies symlink relationships between files by examining each file to see if it's a symlink
//    and determining its target
// 3. Filters out files that are targets of symlinks (they don't need to be included since they're
//    accessible through the symlink)
// 4. Filters out files that are accessible through symlinks using IsAccessibleThroughSymlink
//
// This approach prevents duplication of files in layers when they're already accessible through symlinks,
// which improves layer efficiency and reduces image size.
//
// Returns a filtered list of files to be included in the snapshot.
func (c *CopyCommand) FilesToSnapshot() []string {
	// If no files were changed, return an empty array
	if len(c.snapshotFiles) == 0 {
		return []string{}
	}

	logrus.Debugf("CopyCommand.FilesToSnapshot called with %d files", len(c.snapshotFiles))
	for _, file := range c.snapshotFiles {
		logrus.Debugf("CopyCommand.FilesToSnapshot: file=%s", file)
	}

	// Create a map of all files in the snapshot for symlink accessibility checks
	allFiles := make(map[string]struct{})
	for _, file := range c.snapshotFiles {
		allFiles[file] = struct{}{}
	}

	// Build a map of symlink targets to their symlinks
	symlinkTargets := make(map[string]string)
	for _, file := range c.snapshotFiles {
		fi, err := os.Lstat(file)
		if err == nil && util.IsSymlink(fi) {
			linkTarget, err := os.Readlink(file)
			if err == nil {
				// If the target is not absolute, make it absolute
				if !filepath.IsAbs(linkTarget) {
					linkTarget = filepath.Join(filepath.Dir(file), linkTarget)
				}
				linkTarget = filepath.Clean(linkTarget)
				
				// Check if the target is also in our snapshot files
				for _, potentialTarget := range c.snapshotFiles {
					if linkTarget == potentialTarget {
						symlinkTargets[potentialTarget] = file
						logrus.Debugf("Found symlink relationship: %s is a symlink to %s", file, potentialTarget)
						break
					}
				}
			}
		}
	}

	// Filter out files that are targets of symlinks or accessible through symlinks
	var filteredFiles []string
	for _, file := range c.snapshotFiles {
		if symlink, isTarget := symlinkTargets[file]; isTarget {
			logrus.Debugf("FilesToSnapshot: %s is a target of symlink %s, skipping", file, symlink)
			continue
		} else if !util.IsAccessibleThroughSymlink(file, allFiles) {
			filteredFiles = append(filteredFiles, file)
		} else {
			logrus.Debugf("File %s is accessible through a symlink, not adding to snapshot", file)
		}
	}

	logrus.Debugf("CopyCommand.FilesToSnapshot returning %d files", len(filteredFiles))
	return filteredFiles
}

// String returns some information about the command for the image config
func (c *CopyCommand) String() string {
	return c.cmd.String()
}

func (c *CopyCommand) FilesUsedFromContext(config *v1.Config, buildArgs *dockerfile.BuildArgs) ([]string, error) {
	return copyCmdFilesUsedFromContext(config, buildArgs, c.cmd, c.fileContext)
}

func (c *CopyCommand) MetadataOnly() bool {
	return false
}

func (c *CopyCommand) RequiresUnpackedFS() bool {
	return true
}

func (c *CopyCommand) From() string {
	return c.cmd.From
}

func (c *CopyCommand) ShouldCacheOutput() bool {
	return c.shdCache
}

// CacheCommand returns true since this command should be cached
func (c *CopyCommand) CacheCommand(img v1.Image) DockerCommand {
	return &CachingCopyCommand{
		img:         img,
		cmd:         c.cmd,
		fileContext: c.fileContext,
		extractFn:   util.ExtractFile,
	}
}

type CachingCopyCommand struct {
	BaseCommand
	caching
	img            v1.Image
	extractedFiles []string
	cmd            *instructions.CopyCommand
	fileContext    util.FileContext
	extractFn      util.ExtractFunction
}

func (cr *CachingCopyCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	logrus.Infof("Found cached layer, extracting to filesystem")
	var err error

	if cr.img == nil {
		return errors.New(fmt.Sprintf("cached command image is nil %v", cr.String()))
	}

	layers, err := cr.img.Layers()
	if err != nil {
		return errors.Wrapf(err, "retrieve image layers")
	}

	if len(layers) != 1 {
		return errors.New(fmt.Sprintf("expected %d layers but got %d", 1, len(layers)))
	}

	cr.layer = layers[0]
	cr.extractedFiles, err = util.GetFSFromLayers(kConfig.RootDir, layers, util.ExtractFunc(cr.extractFn), util.IncludeWhiteout())

	logrus.Debugf("ExtractedFiles: %s", cr.extractedFiles)
	if err != nil {
		return errors.Wrap(err, "extracting fs from image")
	}

	return nil
}

func (cr *CachingCopyCommand) FilesUsedFromContext(config *v1.Config, buildArgs *dockerfile.BuildArgs) ([]string, error) {
	return copyCmdFilesUsedFromContext(config, buildArgs, cr.cmd, cr.fileContext)
}

func (cr *CachingCopyCommand) FilesToSnapshot() []string {
	// If no files were extracted, return an empty array
	if len(cr.extractedFiles) == 0 {
		return []string{}
	}

	logrus.Debugf("CachingCopyCommand.FilesToSnapshot called with %d files", len(cr.extractedFiles))
	logrus.Debugf("%d files extracted by caching copy command", len(cr.extractedFiles))
	logrus.Tracef("Extracted files: %s", cr.extractedFiles)

	// Create a map of all files in the snapshot for symlink accessibility checks
	allFiles := make(map[string]struct{})
	for _, file := range cr.extractedFiles {
		allFiles[file] = struct{}{}
	}

	// Build a map of symlink targets to their symlinks
	symlinkTargets := make(map[string]string)
	for _, file := range cr.extractedFiles {
		fi, err := os.Lstat(file)
		if err == nil && util.IsSymlink(fi) {
			linkTarget, err := os.Readlink(file)
			if err == nil {
				// If the target is not absolute, make it absolute
				if !filepath.IsAbs(linkTarget) {
					linkTarget = filepath.Join(filepath.Dir(file), linkTarget)
				}
				linkTarget = filepath.Clean(linkTarget)
				
				// Check if the target is also in our extracted files
				for _, potentialTarget := range cr.extractedFiles {
					if linkTarget == potentialTarget {
						symlinkTargets[potentialTarget] = file
						logrus.Debugf("Found symlink relationship: %s is a symlink to %s", file, potentialTarget)
						break
					}
				}
			}
		}
	}

	// Filter out files that are targets of symlinks or accessible through symlinks
	var filteredFiles []string
	for _, file := range cr.extractedFiles {
		if symlink, isTarget := symlinkTargets[file]; isTarget {
			logrus.Debugf("CachingCopyCommand.FilesToSnapshot: %s is a target of symlink %s, skipping", file, symlink)
			continue
		} else if !util.IsAccessibleThroughSymlink(file, allFiles) {
			filteredFiles = append(filteredFiles, file)
		} else {
			logrus.Debugf("File %s is accessible through a symlink, not adding to snapshot", file)
		}
	}

	logrus.Debugf("CachingCopyCommand.FilesToSnapshot returning %d files", len(filteredFiles))
	return filteredFiles
}

func (cr *CachingCopyCommand) MetadataOnly() bool {
	return false
}

func (cr *CachingCopyCommand) String() string {
	if cr.cmd == nil {
		return "nil command"
	}
	return cr.cmd.String()
}

func (cr *CachingCopyCommand) From() string {
	return cr.cmd.From
}

func resolveIfSymlink(destPath string) (string, error) {
	if !filepath.IsAbs(destPath) {
		return "", errors.New("dest path must be abs")
	}

	var nonexistentPaths []string

	newPath := destPath
	for newPath != "/" {
		_, err := os.Lstat(newPath)
		if err != nil {
			if os.IsNotExist(err) {
				dir, file := filepath.Split(newPath)
				newPath = filepath.Clean(dir)
				nonexistentPaths = append(nonexistentPaths, file)
				continue
			} else {
				return "", errors.Wrap(err, "failed to lstat")
			}
		}

		newPath, err = filepath.EvalSymlinks(newPath)
		if err != nil {
			return "", errors.Wrap(err, "failed to eval symlinks")
		}
		break
	}

	for i := len(nonexistentPaths) - 1; i >= 0; i-- {
		newPath = filepath.Join(newPath, nonexistentPaths[i])
	}

	if destPath != newPath {
		logrus.Tracef("Updating destination path from %v to %v due to symlink", destPath, newPath)
	}

	return filepath.Clean(newPath), nil
}

func copyCmdFilesUsedFromContext(
	config *v1.Config, buildArgs *dockerfile.BuildArgs, cmd *instructions.CopyCommand,
	fileContext util.FileContext,
) ([]string, error) {
	if cmd.From != "" {
		fileContext = util.FileContext{Root: filepath.Join(kConfig.KanikoDir, cmd.From)}
	}

	replacementEnvs := buildArgs.ReplacementEnvs(config.Env)

	srcs, _, err := util.ResolveEnvAndWildcards(
		cmd.SourcesAndDest, fileContext, replacementEnvs,
	)
	if err != nil {
		return nil, err
	}

	files := []string{}
	for _, src := range srcs {
		fullPath := filepath.Join(fileContext.Root, src)
		files = append(files, fullPath)
	}

	logrus.Debugf("Using files from context: %v", files)

	return files, nil
}

// AbstractCopyCommand can either be a CopyCommand or a CachingCopyCommand.
type AbstractCopyCommand interface {
	From() string
}

// CastAbstractCopyCommand tries to convert a command to an AbstractCopyCommand.
func CastAbstractCopyCommand(cmd interface{}) (AbstractCopyCommand, bool) {
	switch v := cmd.(type) {
	case *CopyCommand:
		return v, true
	case *CachingCopyCommand:
		return v, true
	}

	return nil, false
}
