// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package agentcredential

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProfileCanonicalPathsAndIsolation(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "missing", "config.toml")
	dataDir := filepath.Join(root, "missing", "data")
	got, err := Profile(configPath, dataDir)
	if err != nil || !validProfile(got) {
		t.Fatalf("profile: %v", err)
	}
	paths := [2]string{configPath, dataDir}
	if runtime.GOOS == "windows" {
		paths[0], paths[1] = strings.ToLower(paths[0]), strings.ToLower(paths[1])
	}
	encoded, _ := json.Marshal(paths)
	digest := sha256.Sum256(encoded)
	if got != hex.EncodeToString(digest[:]) {
		t.Fatal("profile must hash the length-unambiguous JSON pair")
	}
	dirty := filepath.Dir(configPath) + string(filepath.Separator) + "unused" + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(configPath)
	if same, err := Profile(dirty, dataDir); same != got || err != nil {
		t.Fatal("clean equivalent differs")
	}
	for _, paths := range [][2]string{{configPath + "x", dataDir}, {configPath, dataDir + "x"}, {dataDir, configPath}} {
		if different, err := Profile(paths[0], paths[1]); different == got || err != nil {
			t.Fatal("distinct profile aliases another")
		}
	}
	if runtime.GOOS == "windows" {
		if same, err := Profile(strings.ToUpper(configPath), strings.ToUpper(dataDir)); same != got || err != nil {
			t.Fatal("Windows path spelling must be case insensitive")
		}
	}
}

func TestProfileRelativePaths(t *testing.T) {
	a, err := Profile("config.toml", "data")
	if err != nil {
		t.Fatal(err)
	}
	configPath, _ := filepath.Abs("config.toml")
	dataDir, _ := filepath.Abs("data")
	b, err := Profile(configPath, dataDir)
	if err != nil || a != b {
		t.Fatal("absolute and relative equivalent paths differ")
	}
}

func TestProfileRejectsUnusablePathsWithoutExposingThem(t *testing.T) {
	for _, paths := range [][2]string{{"", "data"}, {"config", ""}, {"private\x00config", "data"}, {"config", "private\x00data"}, {"invalid\xffconfig", "data"}} {
		if got, err := Profile(paths[0], paths[1]); got != "" || !sameSentinel(err, ErrInvalidProfile) {
			t.Fatal("invalid path accepted or exposed")
		}
	}
}
