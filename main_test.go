package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeRepoPath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "simple", input: "shell/.zshrc", want: "shell/.zshrc"},
		{name: "trims whitespace", input: "  nvim/init.lua  ", want: "nvim/init.lua"},
		{name: "normalizes dots", input: "a/./b/../c", want: "a/c"},
		{name: "rejects empty", input: "", wantErr: true},
		{name: "rejects parent", input: "../x", wantErr: true},
		{name: "rejects absolute", input: "/etc/passwd", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeRepoPath(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tc.want {
				t.Fatalf("sanitizeRepoPath(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get home dir: %v", err)
	}

	t.Run("expands tilde", func(t *testing.T) {
		got, err := expandPath("~/.config", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := filepath.Join(home, ".config")
		if got != want {
			t.Fatalf("expandPath returned %q, want %q", got, want)
		}
	})

	t.Run("expands DOTFILES override", func(t *testing.T) {
		dotfiles := filepath.Join(t.TempDir(), "mydotfiles")
		got, err := expandPath("$DOTFILES/shell/.zshrc", dotfiles)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := filepath.Join(dotfiles, "shell", ".zshrc")
		if got != want {
			t.Fatalf("expandPath returned %q, want %q", got, want)
		}
	})

	t.Run("rejects unsupported tilde form", func(t *testing.T) {
		if _, err := expandPath("~other/file", ""); err == nil {
			t.Fatalf("expected error for unsupported tilde form")
		}
	})
}

func TestParseMap(t *testing.T) {
	dotfiles := t.TempDir()
	mapPath := filepath.Join(dotfiles, mapFileName)

	content := strings.Join([]string{
		"# shell",
		"",
		"shell/.zshrc : ~/.zshrc",
		"nvim/init.lua : ~/.config/nvim/init.lua",
	}, "\n") + "\n"

	if err := os.WriteFile(mapPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write map file: %v", err)
	}

	mappings, err := parseMap(mapPath, dotfiles)
	if err != nil {
		t.Fatalf("parseMap returned error: %v", err)
	}

	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(mappings))
	}

	if mappings[0].RepoRel != "shell/.zshrc" {
		t.Fatalf("unexpected first RepoRel: %q", mappings[0].RepoRel)
	}

	if mappings[0].SystemRaw != "~/.zshrc" {
		t.Fatalf("unexpected first SystemRaw: %q", mappings[0].SystemRaw)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get home dir: %v", err)
	}

	wantSystemAbs := filepath.Join(home, ".zshrc")
	if mappings[0].SystemAbs != wantSystemAbs {
		t.Fatalf("unexpected first SystemAbs: %q (want %q)", mappings[0].SystemAbs, wantSystemAbs)
	}

	t.Run("invalid line returns error", func(t *testing.T) {
		badPath := filepath.Join(dotfiles, "bad.map")
		if err := os.WriteFile(badPath, []byte("invalid-line\n"), 0o644); err != nil {
			t.Fatalf("failed to write bad map: %v", err)
		}

		if _, err := parseMap(badPath, dotfiles); err == nil {
			t.Fatalf("expected parse error for invalid line")
		}
	})
}

func TestIgnoreMatcherMatch(t *testing.T) {
	packageDir := t.TempDir()
	ignorePath := filepath.Join(packageDir, compatIgnoreFileName)
	content := strings.Join([]string{
		"# comments are ignored",
		"README.*",
		"^/docs/.*",
		"foo\\#bar # keep escaped hash",
	}, "\n") + "\n"

	if err := os.WriteFile(ignorePath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write ignore file: %v", err)
	}

	matcher, err := loadIgnoreMatcher(packageDir)
	if err != nil {
		t.Fatalf("loadIgnoreMatcher returned error: %v", err)
	}
	if matcher == nil {
		t.Fatalf("expected ignore matcher to be loaded")
	}

	tests := []struct {
		path string
		want bool
	}{
		{path: "README.md", want: true},
		{path: "docs/init.lua", want: true},
		{path: "foo#bar", want: true},
		{path: localIgnoreFileName, want: true},
		{path: compatIgnoreFileName, want: true},
		{path: "lua/plugins.lua", want: false},
	}

	for _, tc := range tests {
		if got := matcher.Match(tc.path); got != tc.want {
			t.Fatalf("matcher.Match(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSymlinkPointsTo(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("failed to create target file: %v", err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	ok, err := symlinkPointsTo(link, target)
	if err != nil {
		t.Fatalf("symlinkPointsTo returned error: %v", err)
	}
	if !ok {
		t.Fatalf("expected symlink to point to target")
	}

	other := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(other, []byte("y"), 0o644); err != nil {
		t.Fatalf("failed to create other file: %v", err)
	}

	ok, err = symlinkPointsTo(link, other)
	if err != nil {
		t.Fatalf("symlinkPointsTo returned error: %v", err)
	}
	if ok {
		t.Fatalf("expected symlink not to point to other target")
	}
}

func TestMappingStatus(t *testing.T) {
	t.Run("MISSING", func(t *testing.T) {
		dotfiles := t.TempDir()
		systemPath := filepath.Join(dotfiles, "home", ".zshrc")
		m := mapping{RepoRel: "shell/.zshrc", SystemRaw: systemPath, SystemAbs: systemPath}

		got, err := mappingStatus(dotfiles, m)
		if err != nil {
			t.Fatalf("mappingStatus error: %v", err)
		}
		if got != "MISSING" {
			t.Fatalf("got %q, want MISSING", got)
		}
	})

	t.Run("STRAY regular file", func(t *testing.T) {
		dotfiles := t.TempDir()
		systemPath := filepath.Join(dotfiles, "home", ".zshrc")
		if err := os.MkdirAll(filepath.Dir(systemPath), 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		if err := os.WriteFile(systemPath, []byte("stray"), 0o644); err != nil {
			t.Fatalf("write failed: %v", err)
		}

		m := mapping{RepoRel: "shell/.zshrc", SystemRaw: systemPath, SystemAbs: systemPath}
		got, err := mappingStatus(dotfiles, m)
		if err != nil {
			t.Fatalf("mappingStatus error: %v", err)
		}
		if got != "STRAY" {
			t.Fatalf("got %q, want STRAY", got)
		}
	})

	t.Run("BROKEN", func(t *testing.T) {
		dotfiles := t.TempDir()
		repoRel := "shell/.zshrc"
		repoAbs := repoAbsPath(dotfiles, repoRel)
		systemPath := filepath.Join(dotfiles, "home", ".zshrc")

		if err := os.MkdirAll(filepath.Dir(systemPath), 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		if err := os.Symlink(repoAbs, systemPath); err != nil {
			t.Fatalf("symlink failed: %v", err)
		}

		m := mapping{RepoRel: repoRel, SystemRaw: systemPath, SystemAbs: systemPath}
		got, err := mappingStatus(dotfiles, m)
		if err != nil {
			t.Fatalf("mappingStatus error: %v", err)
		}
		if got != "BROKEN" {
			t.Fatalf("got %q, want BROKEN", got)
		}
	})

	t.Run("OK", func(t *testing.T) {
		dotfiles := t.TempDir()
		repoRel := "shell/.zshrc"
		repoAbs := repoAbsPath(dotfiles, repoRel)
		systemPath := filepath.Join(dotfiles, "home", ".zshrc")

		if err := os.MkdirAll(filepath.Dir(repoAbs), 0o755); err != nil {
			t.Fatalf("mkdir repo failed: %v", err)
		}
		if err := os.WriteFile(repoAbs, []byte("ok"), 0o644); err != nil {
			t.Fatalf("write repo file failed: %v", err)
		}

		if err := os.MkdirAll(filepath.Dir(systemPath), 0o755); err != nil {
			t.Fatalf("mkdir system failed: %v", err)
		}
		if err := os.Symlink(repoAbs, systemPath); err != nil {
			t.Fatalf("symlink failed: %v", err)
		}

		m := mapping{RepoRel: repoRel, SystemRaw: systemPath, SystemAbs: systemPath}
		got, err := mappingStatus(dotfiles, m)
		if err != nil {
			t.Fatalf("mappingStatus error: %v", err)
		}
		if got != "OK" {
			t.Fatalf("got %q, want OK", got)
		}
	})
}

func TestAppendMapping(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, mapFileName)

	if err := appendMapping(mapPath, "shell/.zshrc", "~/.zshrc"); err != nil {
		t.Fatalf("appendMapping first call failed: %v", err)
	}
	if err := appendMapping(mapPath, "nvim/init.lua", "~/.config/nvim/init.lua"); err != nil {
		t.Fatalf("appendMapping second call failed: %v", err)
	}

	data, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("failed to read map file: %v", err)
	}

	text := string(data)
	if !strings.Contains(text, "shell/.zshrc : ~/.zshrc\n") {
		t.Fatalf("missing first mapping in map file: %q", text)
	}
	if !strings.Contains(text, "nvim/init.lua : ~/.config/nvim/init.lua\n") {
		t.Fatalf("missing second mapping in map file: %q", text)
	}
}

func TestParseGlobalOptions(t *testing.T) {
	t.Run("parses simulate flag", func(t *testing.T) {
		options, args := parseGlobalOptions([]string{"--simulate", "track", "~/.bashrc"})
		if !options.Simulate {
			t.Fatalf("expected simulate option to be true")
		}

		if len(args) != 2 || args[0] != "track" || args[1] != "~/.bashrc" {
			t.Fatalf("unexpected parsed args: %v", args)
		}
	})

	t.Run("parses short simulate flag after command", func(t *testing.T) {
		options, args := parseGlobalOptions([]string{"link", "-n"})
		if !options.Simulate {
			t.Fatalf("expected simulate option to be true")
		}

		if len(args) != 1 || args[0] != "link" {
			t.Fatalf("unexpected parsed args: %v", args)
		}
	})

	t.Run("respects option terminator", func(t *testing.T) {
		options, args := parseGlobalOptions([]string{"track", "--", "--simulate", "shell/.zshrc"})
		if options.Simulate {
			t.Fatalf("expected simulate option to remain false")
		}

		if len(args) != 3 || args[0] != "track" || args[1] != "--simulate" || args[2] != "shell/.zshrc" {
			t.Fatalf("unexpected parsed args: %v", args)
		}
	})
}

func TestIntegrationTrackAndLink(t *testing.T) {
	root, err := os.MkdirTemp("", "dot-integration-")
	if err != nil {
		t.Fatalf("failed to create temp root: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(root)
	}()

	dotfilesDir := filepath.Join(root, "dotfiles")
	homeDir := filepath.Join(root, "home")
	systemFile := filepath.Join(homeDir, ".bashrc")

	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("failed to create temp home dir: %v", err)
	}

	originalContent := []byte("export TEST_VAR=1\n")
	if err := os.WriteFile(systemFile, originalContent, 0o644); err != nil {
		t.Fatalf("failed to create source file: %v", err)
	}

	if err := cmdTrack(dotfilesDir, []string{systemFile, "shell/.bashrc"}); err != nil {
		t.Fatalf("cmdTrack failed: %v", err)
	}

	repoFile := filepath.Join(dotfilesDir, "shell", ".bashrc")

	repoData, err := os.ReadFile(repoFile)
	if err != nil {
		t.Fatalf("failed to read tracked repo file: %v", err)
	}
	if string(repoData) != string(originalContent) {
		t.Fatalf("repo file content mismatch: got %q, want %q", string(repoData), string(originalContent))
	}

	ok, err := symlinkPointsTo(systemFile, repoFile)
	if err != nil {
		t.Fatalf("failed to inspect created symlink: %v", err)
	}
	if !ok {
		t.Fatalf("system file symlink does not point to repo file")
	}

	if err := os.Remove(systemFile); err != nil {
		t.Fatalf("failed to remove symlink before relink test: %v", err)
	}

	if err := cmdLink(dotfilesDir); err != nil {
		t.Fatalf("cmdLink failed: %v", err)
	}

	ok, err = symlinkPointsTo(systemFile, repoFile)
	if err != nil {
		t.Fatalf("failed to inspect relinked symlink: %v", err)
	}
	if !ok {
		t.Fatalf("relinked system file does not point to repo file")
	}

	mapPath := filepath.Join(dotfilesDir, mapFileName)
	mapData, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("failed to read map file: %v", err)
	}

	if !strings.Contains(string(mapData), "shell/.bashrc : ") {
		t.Fatalf("map file is missing track entry: %q", string(mapData))
	}
}

func TestIntegrationSimulateTrack(t *testing.T) {
	root := t.TempDir()
	dotfilesDir := filepath.Join(root, "dotfiles")
	homeDir := filepath.Join(root, "home")
	systemFile := filepath.Join(homeDir, ".bashrc")

	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("failed to create temp home dir: %v", err)
	}

	originalContent := []byte("export TEST_VAR=1\n")
	if err := os.WriteFile(systemFile, originalContent, 0o644); err != nil {
		t.Fatalf("failed to create source file: %v", err)
	}

	t.Setenv("DOTFILES", dotfilesDir)

	if err := run([]string{"--simulate", "track", systemFile, "shell/.bashrc"}); err != nil {
		t.Fatalf("run simulate track failed: %v", err)
	}

	info, err := os.Lstat(systemFile)
	if err != nil {
		t.Fatalf("failed to inspect source file after simulate: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("source file should remain a regular file in simulate mode")
	}

	data, err := os.ReadFile(systemFile)
	if err != nil {
		t.Fatalf("failed to read source file after simulate: %v", err)
	}
	if string(data) != string(originalContent) {
		t.Fatalf("source file content changed in simulate mode: got %q, want %q", string(data), string(originalContent))
	}

	repoFile := filepath.Join(dotfilesDir, "shell", ".bashrc")
	if _, err := os.Stat(repoFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repo file should not exist after simulate track, err=%v", err)
	}

	mapPath := filepath.Join(dotfilesDir, mapFileName)
	if _, err := os.Stat(mapPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("map file should not exist after simulate track, err=%v", err)
	}
}

func TestIntegrationSimulateLink(t *testing.T) {
	root := t.TempDir()
	dotfilesDir := filepath.Join(root, "dotfiles")
	homeDir := filepath.Join(root, "home")
	systemFile := filepath.Join(homeDir, ".bashrc")
	repoFile := filepath.Join(dotfilesDir, "shell", ".bashrc")

	if err := os.MkdirAll(filepath.Dir(repoFile), 0o755); err != nil {
		t.Fatalf("failed to create repo directory: %v", err)
	}
	if err := os.WriteFile(repoFile, []byte("content\n"), 0o644); err != nil {
		t.Fatalf("failed to write repo file: %v", err)
	}

	mapPath := filepath.Join(dotfilesDir, mapFileName)
	mapContent := "shell/.bashrc : " + systemFile + "\n"
	if err := os.WriteFile(mapPath, []byte(mapContent), 0o644); err != nil {
		t.Fatalf("failed to write map file: %v", err)
	}

	t.Setenv("DOTFILES", dotfilesDir)

	if err := run([]string{"link", "--simulate"}); err != nil {
		t.Fatalf("run simulate link failed: %v", err)
	}

	if _, err := os.Lstat(systemFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("system file should not be created in simulate mode, err=%v", err)
	}
}

func TestIntegrationSimulateSync(t *testing.T) {
	dotfilesDir := t.TempDir()
	t.Setenv("DOTFILES", dotfilesDir)

	if err := run([]string{"--simulate", "sync"}); err != nil {
		t.Fatalf("run simulate sync failed: %v", err)
	}
}

func TestIntegrationTrackDirectoryAndLink(t *testing.T) {
	root, err := os.MkdirTemp("", "dot-dir-integration-")
	if err != nil {
		t.Fatalf("failed to create temp root: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(root)
	}()

	dotfilesDir := filepath.Join(root, "dotfiles")
	homeDir := filepath.Join(root, "home")
	systemDir := filepath.Join(homeDir, ".config", "nvim")
	nestedSystemFile := filepath.Join(systemDir, "lua", "plugins.lua")

	if err := os.MkdirAll(filepath.Dir(nestedSystemFile), 0o755); err != nil {
		t.Fatalf("failed to create nested source directory: %v", err)
	}

	originalContent := []byte("return {}\n")
	if err := os.WriteFile(nestedSystemFile, originalContent, 0o644); err != nil {
		t.Fatalf("failed to create nested source file: %v", err)
	}

	if err := cmdTrack(dotfilesDir, []string{systemDir, "nvim"}); err != nil {
		t.Fatalf("cmdTrack failed for directory: %v", err)
	}

	repoDir := filepath.Join(dotfilesDir, "nvim")
	repoNestedFile := filepath.Join(repoDir, "lua", "plugins.lua")

	repoData, err := os.ReadFile(repoNestedFile)
	if err != nil {
		t.Fatalf("failed to read tracked nested repo file: %v", err)
	}
	if string(repoData) != string(originalContent) {
		t.Fatalf("repo nested file content mismatch: got %q, want %q", string(repoData), string(originalContent))
	}

	ok, err := symlinkPointsTo(systemDir, repoDir)
	if err != nil {
		t.Fatalf("failed to inspect created directory symlink: %v", err)
	}
	if !ok {
		t.Fatalf("system directory symlink does not point to repo directory")
	}

	throughLinkData, err := os.ReadFile(filepath.Join(systemDir, "lua", "plugins.lua"))
	if err != nil {
		t.Fatalf("failed to read nested file through symlink: %v", err)
	}
	if string(throughLinkData) != string(originalContent) {
		t.Fatalf("nested file through symlink mismatch: got %q, want %q", string(throughLinkData), string(originalContent))
	}

	if err := os.Remove(systemDir); err != nil {
		t.Fatalf("failed to remove directory symlink before relink test: %v", err)
	}

	if err := cmdLink(dotfilesDir); err != nil {
		t.Fatalf("cmdLink failed for directory mapping: %v", err)
	}

	ok, err = symlinkPointsTo(systemDir, repoDir)
	if err != nil {
		t.Fatalf("failed to inspect relinked directory symlink: %v", err)
	}
	if !ok {
		t.Fatalf("relinked system directory does not point to repo directory")
	}
}

func TestIntegrationLinkDirectoryWithIgnoreFile(t *testing.T) {
	root := t.TempDir()
	dotfilesDir := filepath.Join(root, "dotfiles")
	homeDir := filepath.Join(root, "home")
	systemDir := filepath.Join(homeDir, ".config", "nvim")
	repoDir := filepath.Join(dotfilesDir, "nvim")
	repoInit := filepath.Join(repoDir, "init.lua")
	repoPlugins := filepath.Join(repoDir, "lua", "plugins.lua")

	if err := os.MkdirAll(filepath.Dir(repoPlugins), 0o755); err != nil {
		t.Fatalf("failed to create repo directories: %v", err)
	}
	if err := os.WriteFile(repoInit, []byte("vim.o.number = true\n"), 0o644); err != nil {
		t.Fatalf("failed to write repo init.lua: %v", err)
	}
	if err := os.WriteFile(repoPlugins, []byte("return {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write repo plugins.lua: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("docs\n"), 0o644); err != nil {
		t.Fatalf("failed to write repo README.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, localIgnoreFileName), []byte("^/README.*\n"), 0o644); err != nil {
		t.Fatalf("failed to write ignore file: %v", err)
	}

	mapPath := filepath.Join(dotfilesDir, mapFileName)
	mapContent := "nvim : " + systemDir + "\n"
	if err := os.WriteFile(mapPath, []byte(mapContent), 0o644); err != nil {
		t.Fatalf("failed to write map file: %v", err)
	}

	if err := cmdLink(dotfilesDir); err != nil {
		t.Fatalf("cmdLink failed: %v", err)
	}

	info, err := os.Lstat(systemDir)
	if err != nil {
		t.Fatalf("failed to inspect linked system directory: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("system directory should be a real directory when ignore file is present")
	}

	ok, err := symlinkPointsTo(filepath.Join(systemDir, "init.lua"), repoInit)
	if err != nil {
		t.Fatalf("failed to inspect init.lua symlink: %v", err)
	}
	if !ok {
		t.Fatalf("init.lua symlink does not point to repo file")
	}

	nestedInfo, err := os.Lstat(filepath.Join(systemDir, "lua"))
	if err != nil {
		t.Fatalf("failed to inspect nested linked directory: %v", err)
	}
	if !nestedInfo.IsDir() || nestedInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("nested directory should be a real directory when ignore file is present")
	}

	ok, err = symlinkPointsTo(filepath.Join(systemDir, "lua", "plugins.lua"), repoPlugins)
	if err != nil {
		t.Fatalf("failed to inspect plugins.lua symlink: %v", err)
	}
	if !ok {
		t.Fatalf("plugins.lua symlink does not point to repo file")
	}

	if _, err := os.Lstat(filepath.Join(systemDir, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("README.md should be ignored, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(systemDir, localIgnoreFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignore file should never be linked, err=%v", err)
	}

	status, err := mappingStatus(dotfilesDir, mapping{RepoRel: "nvim", SystemRaw: systemDir, SystemAbs: systemDir})
	if err != nil {
		t.Fatalf("mappingStatus returned error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("mappingStatus = %q, want OK", status)
	}
}

func TestIntegrationTrackDirectoryWithIgnoreFile(t *testing.T) {
	root := t.TempDir()
	dotfilesDir := filepath.Join(root, "dotfiles")
	homeDir := filepath.Join(root, "home")
	systemDir := filepath.Join(homeDir, ".config", "nvim")
	systemInit := filepath.Join(systemDir, "init.lua")

	if err := os.MkdirAll(systemDir, 0o755); err != nil {
		t.Fatalf("failed to create system directory: %v", err)
	}
	if err := os.WriteFile(systemInit, []byte("vim.o.number = true\n"), 0o644); err != nil {
		t.Fatalf("failed to write system init.lua: %v", err)
	}
	if err := os.WriteFile(filepath.Join(systemDir, "README.md"), []byte("docs\n"), 0o644); err != nil {
		t.Fatalf("failed to write system README.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(systemDir, localIgnoreFileName), []byte("^/README.*\n"), 0o644); err != nil {
		t.Fatalf("failed to write ignore file: %v", err)
	}

	if err := cmdTrack(dotfilesDir, []string{systemDir, "nvim"}); err != nil {
		t.Fatalf("cmdTrack failed: %v", err)
	}

	repoDir := filepath.Join(dotfilesDir, "nvim")
	repoInit := filepath.Join(repoDir, "init.lua")
	if _, err := os.Stat(filepath.Join(repoDir, "README.md")); err != nil {
		t.Fatalf("tracked repo should keep ignored README.md: %v", err)
	}

	info, err := os.Lstat(systemDir)
	if err != nil {
		t.Fatalf("failed to inspect tracked system directory: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("tracked system directory should be a real directory when ignore file is present")
	}

	ok, err := symlinkPointsTo(filepath.Join(systemDir, "init.lua"), repoInit)
	if err != nil {
		t.Fatalf("failed to inspect tracked init.lua symlink: %v", err)
	}
	if !ok {
		t.Fatalf("tracked init.lua symlink does not point to repo file")
	}

	if _, err := os.Lstat(filepath.Join(systemDir, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("README.md should be ignored after track, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(systemDir, localIgnoreFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignore file should not be linked after track, err=%v", err)
	}

	status, err := mappingStatus(dotfilesDir, mapping{RepoRel: "nvim", SystemRaw: systemDir, SystemAbs: systemDir})
	if err != nil {
		t.Fatalf("mappingStatus returned error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("mappingStatus = %q, want OK", status)
	}
}
