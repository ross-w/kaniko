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
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
)

func Test_CacheKey(t *testing.T) {
	tests := []struct {
		name  string
		map1  map[string]string
		map2  map[string]string
		equal bool
	}{
		{
			name: "maps are the same",
			map1: map[string]string{
				"a": "apple",
				"b": "bat",
				"c": "cat",
				"d": "dog",
				"e": "egg",
			},
			map2: map[string]string{
				"c": "cat",
				"d": "dog",
				"b": "bat",
				"a": "apple",
				"e": "egg",
			},
			equal: true,
		},
		{
			name: "maps are different",
			map1: map[string]string{
				"a": "apple",
				"b": "bat",
				"c": "cat",
			},
			map2: map[string]string{
				"c": "",
				"b": "bat",
				"a": "apple",
			},
			equal: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lm1 := LayeredMap{adds: []map[string]string{test.map1}, deletes: []map[string]struct{}{nil, nil}}
			lm2 := LayeredMap{adds: []map[string]string{test.map2}, deletes: []map[string]struct{}{nil, nil}}
			k1, err := lm1.Key()
			if err != nil {
				t.Fatalf("error getting key for map 1: %v", err)
			}
			k2, err := lm2.Key()
			if err != nil {
				t.Fatalf("error getting key for map 2: %v", err)
			}
			if test.equal != (k1 == k2) {
				t.Fatalf("unexpected result: \nExpected\n%s\nActual\n%s\n", k1, k2)
			}
		})
	}
}

func Test_FlattenPaths(t *testing.T) {
	layers := []map[string]string{
		{
			"a": "2",
			"b": "3",
		},
		{
			"b": "5",
			"c": "6",
		},
		{
			"a": "8",
		},
	}

	whiteouts := []map[string]struct{}{
		{
			"a": {}, // delete a
		},
		{
			"b": {}, // delete b
		},
		{
			"c": {}, // delete c
		},
	}

	lm := LayeredMap{
		adds:    []map[string]string{layers[0]},
		deletes: []map[string]struct{}{whiteouts[0]}}

	paths := lm.GetCurrentPaths()

	assertPath := func(f string, exists bool) {
		_, ok := paths[f]
		if exists && !ok {
			t.Fatalf("expected path '%s' to be present.", f)
		} else if !exists && ok {
			t.Fatalf("expected path '%s' not to be present.", f)
		}
	}

	assertPath("a", false)
	assertPath("b", true)

	lm = LayeredMap{
		adds:    []map[string]string{layers[0], layers[1]},
		deletes: []map[string]struct{}{whiteouts[0], whiteouts[1]}}
	paths = lm.GetCurrentPaths()

	assertPath("a", false)
	assertPath("b", false)
	assertPath("c", true)

	lm = LayeredMap{
		adds:    []map[string]string{layers[0], layers[1], layers[2]},
		deletes: []map[string]struct{}{whiteouts[0], whiteouts[1], whiteouts[2]}}
	paths = lm.GetCurrentPaths()

	assertPath("a", true)
	assertPath("b", false)
	assertPath("c", false)
}

func Test_isAccessibleThroughSymlink(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := ioutil.TempDir("", "layered-map-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a test file
	testFilePath := filepath.Join(tempDir, "test-file")
	if err := ioutil.WriteFile(testFilePath, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create a symlink to the test file
	symlinkPath := filepath.Join(tempDir, "test-symlink")
	if err := os.Symlink(testFilePath, symlinkPath); err != nil {
		t.Fatalf("Failed to create symlink: %v", err)
	}

	// Create a LayeredMap with the test file and symlink
	lm := NewLayeredMap(func(s string) (string, error) {
		return "dummy-hash", nil
	})
	lm.Snapshot()

	// Add the test file and symlink to the current image
	lm.currentImage = map[string]string{
		testFilePath: "hash1",
		symlinkPath:  "hash2",
	}

	// Test that the file is accessible through the symlink
	if !lm.isAccessibleThroughSymlink(testFilePath) {
		t.Errorf("Expected file to be accessible through symlink, but it wasn't")
	}

	// Create another file that is not linked
	unlinkedFilePath := filepath.Join(tempDir, "unlinked-file")
	if err := ioutil.WriteFile(unlinkedFilePath, []byte("unlinked content"), 0644); err != nil {
		t.Fatalf("Failed to create unlinked file: %v", err)
	}

	// Add the unlinked file to the current image
	lm.currentImage[unlinkedFilePath] = "hash3"

	// Test that the unlinked file is not accessible through any symlink
	if lm.isAccessibleThroughSymlink(unlinkedFilePath) {
		t.Errorf("Expected unlinked file to not be accessible through symlink, but it was")
	}
}

func Test_CheckFileChange_WithSymlinks(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := ioutil.TempDir("", "layered-map-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a test file
	testFilePath := filepath.Join(tempDir, "test-file")
	if err := ioutil.WriteFile(testFilePath, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create a symlink to the test file
	symlinkPath := filepath.Join(tempDir, "test-symlink")
	if err := os.Symlink(testFilePath, symlinkPath); err != nil {
		t.Fatalf("Failed to create symlink: %v", err)
	}

	// Create a LayeredMap with a mock hasher that always returns the same hash
	lm := NewLayeredMap(func(s string) (string, error) {
		return "dummy-hash", nil
	})
	lm.Snapshot()

	// Add the symlink to the current image
	lm.currentImage = map[string]string{
		symlinkPath: "symlink-hash",
	}

	// Test that CheckFileChange returns false (unchanged) for the test file
	// because it's accessible through the symlink
	changed, err := lm.CheckFileChange(testFilePath)
	if err != nil {
		t.Fatalf("CheckFileChange failed: %v", err)
	}
	if changed {
		t.Errorf("Expected CheckFileChange to return false (unchanged) for file accessible through symlink, but got true")
	}

	// Create another file that is not linked
	unlinkedFilePath := filepath.Join(tempDir, "unlinked-file")
	if err := ioutil.WriteFile(unlinkedFilePath, []byte("unlinked content"), 0644); err != nil {
		t.Fatalf("Failed to create unlinked file: %v", err)
	}

	// Test that CheckFileChange returns true (changed) for the unlinked file
	changed, err = lm.CheckFileChange(unlinkedFilePath)
	if err != nil {
		t.Fatalf("CheckFileChange failed: %v", err)
	}
	if !changed {
		t.Errorf("Expected CheckFileChange to return true (changed) for unlinked file, but got false")
	}
}
