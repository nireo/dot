package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const mapFileName = ".dot.map"
const localIgnoreFileName = ".dot-local-ignore"
const compatIgnoreFileName = ".stow-local-ignore"

type mapping struct {
	RepoRel   string
	SystemRaw string
	SystemAbs string
	Line      int
}

type globalOptions struct {
	Simulate bool
}

type ignoreMatcher struct {
	basename []*regexp.Regexp
	path     []*regexp.Regexp
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "dot: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	options, commandArgs := parseGlobalOptions(args)

	if len(commandArgs) == 0 {
		printUsage()
		return nil
	}

	dotfilesDir, err := resolveDotfilesDir()
	if err != nil {
		return fmt.Errorf("resolve DOTFILES: %w", err)
	}

	switch commandArgs[0] {
	case "track":
		return cmdTrackWithSimulate(dotfilesDir, commandArgs[1:], options.Simulate)
	case "link":
		if len(commandArgs) != 1 {
			return errors.New("usage: dot link")
		}
		return cmdLinkWithSimulate(dotfilesDir, options.Simulate)
	case "list":
		if len(commandArgs) != 1 {
			return errors.New("usage: dot list")
		}
		return cmdList(dotfilesDir)
	case "sync":
		if len(commandArgs) != 1 {
			return errors.New("usage: dot sync")
		}
		return cmdSyncWithSimulate(dotfilesDir, options.Simulate)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", commandArgs[0])
	}
}

func parseGlobalOptions(args []string) (globalOptions, []string) {
	options := globalOptions{}
	commandArgs := make([]string, 0, len(args))
	parsingOptions := true

	for _, arg := range args {
		if parsingOptions && arg == "--" {
			parsingOptions = false
			continue
		}

		if parsingOptions {
			switch arg {
			case "-n", "--simulate":
				options.Simulate = true
				continue
			}
		}

		commandArgs = append(commandArgs, arg)
	}

	return options, commandArgs
}

func cmdTrack(dotfilesDir string, args []string) error {
	return cmdTrackWithSimulate(dotfilesDir, args, false)
}

func cmdTrackWithSimulate(dotfilesDir string, args []string, simulate bool) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: dot track <file> [target_path]")
	}

	sourceAbs, err := expandPath(args[0], dotfilesDir)
	if err != nil {
		return fmt.Errorf("resolve source path: %w", err)
	}

	info, err := os.Lstat(sourceAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("source file does not exist: %s", sourceAbs)
		}
		return fmt.Errorf("inspect source file: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source path is a symlink (track expects a regular file or directory): %s", sourceAbs)
	}

	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("source path must be a regular file or directory: %s", sourceAbs)
	}

	repoRel := ""
	if len(args) == 2 {
		repoRel, err = sanitizeRepoPath(args[1])
		if err != nil {
			return fmt.Errorf("invalid target_path: %w", err)
		}
	} else {
		repoRel, err = sanitizeRepoPath(filepath.Base(sourceAbs))
		if err != nil {
			return fmt.Errorf("invalid source file name: %w", err)
		}
	}

	mapPath := filepath.Join(dotfilesDir, mapFileName)
	mappings, err := parseMap(mapPath, dotfilesDir)
	if err != nil {
		return err
	}

	for _, m := range mappings {
		if m.SystemAbs == sourceAbs {
			if m.RepoRel == repoRel {
				return fmt.Errorf("already tracked: %s -> %s", sourceAbs, repoRel)
			}
			return fmt.Errorf("system path already tracked to %s (line %d)", m.RepoRel, m.Line)
		}

		if m.RepoRel == repoRel && m.SystemAbs != sourceAbs {
			return fmt.Errorf("target_path already used by %s (line %d)", m.SystemRaw, m.Line)
		}
	}

	repoAbs := repoAbsPath(dotfilesDir, repoRel)
	if stat, statErr := os.Lstat(repoAbs); statErr == nil {
		if stat.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("target_path already exists as symlink: %s", repoAbs)
		}
		return fmt.Errorf("target_path already exists: %s", repoAbs)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect target_path: %w", statErr)
	}

	if simulate {
		parentDir := filepath.Dir(repoAbs)
		if info, err := os.Stat(parentDir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Printf("Simulate: would create directory %s\n", parentDir)
			} else {
				return fmt.Errorf("inspect target_path directory: %w", err)
			}
		} else if !info.IsDir() {
			return fmt.Errorf("target_path directory is not a directory: %s", parentDir)
		}

		fmt.Printf("Simulate: would move %s -> %s\n", sourceAbs, repoAbs)

		ignoreMatcher, err := ignoreMatcherForDir(sourceAbs, info)
		if err != nil {
			return err
		}

		if ignoreMatcher != nil {
			if err := simulateIgnoredDirectoryLayout(sourceAbs, sourceAbs, ignoreMatcher); err != nil {
				return err
			}
		} else {
			fmt.Printf("Simulate: would create symlink %s -> %s\n", sourceAbs, repoAbs)
		}

		fmt.Printf("Simulate: would append mapping %s : %s\n", repoRel, compressHome(sourceAbs))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(repoAbs), 0o755); err != nil {
		return fmt.Errorf("create target_path directory: %w", err)
	}

	if err := movePath(sourceAbs, repoAbs); err != nil {
		return fmt.Errorf("move source into DOTFILES: %w", err)
	}

	if _, err := os.Stat(repoAbs); err != nil {
		rollbackErr := movePath(repoAbs, sourceAbs)
		if rollbackErr != nil {
			return fmt.Errorf("move verification failed (%v) and rollback failed (%v)", err, rollbackErr)
		}
		return fmt.Errorf("move verification failed: %w", err)
	}

	ignoreMatcher, err := ignoreMatcherForDir(repoAbs, info)
	if err != nil {
		rollbackErr := movePath(repoAbs, sourceAbs)
		if rollbackErr != nil {
			return fmt.Errorf("load ignore file failed (%v) and rollback failed (%v)", err, rollbackErr)
		}
		return err
	}

	if err := linkTrackedPath(repoAbs, sourceAbs, ignoreMatcher, false); err != nil {
		removeErr := os.RemoveAll(sourceAbs)
		rollbackErr := movePath(repoAbs, sourceAbs)
		if rollbackErr != nil {
			return fmt.Errorf("create symlink failed (%v), rollback remove link layout (%v), rollback move file (%v)", err, removeErr, rollbackErr)
		}
		return fmt.Errorf("create symlink: %w", err)
	}

	if err := appendMapping(mapPath, repoRel, compressHome(sourceAbs)); err != nil {
		removeErr := os.RemoveAll(sourceAbs)
		rollbackErr := movePath(repoAbs, sourceAbs)
		if removeErr != nil || rollbackErr != nil {
			return fmt.Errorf("write map failed (%v), rollback remove symlink (%v), rollback move file (%v)", err, removeErr, rollbackErr)
		}
		return fmt.Errorf("write map: %w", err)
	}

	fmt.Printf("Tracked %s -> %s\n", sourceAbs, repoRel)
	return nil
}

func cmdLink(dotfilesDir string) error {
	return cmdLinkWithSimulate(dotfilesDir, false)
}

func cmdLinkWithSimulate(dotfilesDir string, simulate bool) error {
	mapPath := filepath.Join(dotfilesDir, mapFileName)
	mappings, err := parseMap(mapPath, dotfilesDir)
	if err != nil {
		return err
	}

	if len(mappings) == 0 {
		fmt.Printf("No mappings found in %s\n", mapPath)
		return nil
	}

	conflicts := 0
	for _, m := range mappings {
		mappingConflicts, err := linkMapping(dotfilesDir, m, simulate)
		if err != nil {
			return err
		}
		conflicts += mappingConflicts
	}

	if conflicts > 0 {
		if simulate {
			fmt.Fprintf(os.Stderr, "Simulate: would skip %d conflict(s).\n", conflicts)
		} else {
			fmt.Fprintf(os.Stderr, "Skipped %d conflict(s).\n", conflicts)
		}
	}

	return nil
}

func cmdList(dotfilesDir string) error {
	mapPath := filepath.Join(dotfilesDir, mapFileName)
	mappings, err := parseMap(mapPath, dotfilesDir)
	if err != nil {
		return err
	}

	if len(mappings) == 0 {
		fmt.Printf("No mappings found in %s\n", mapPath)
		return nil
	}

	for _, m := range mappings {
		status, err := mappingStatus(dotfilesDir, m)
		if err != nil {
			return fmt.Errorf("status for line %d: %w", m.Line, err)
		}

		fmt.Printf("%-8s %s : %s\n", status, m.RepoRel, m.SystemRaw)
	}

	return nil
}

func cmdSync(dotfilesDir string) error {
	return cmdSyncWithSimulate(dotfilesDir, false)
}

func cmdSyncWithSimulate(dotfilesDir string, simulate bool) error {
	if simulate {
		fmt.Println("Simulate: would run git add .")
		fmt.Println("Simulate: would prompt for commit message")
		fmt.Println("Simulate: would run git commit -m <message>")
		fmt.Println("Simulate: would run git push")
		return nil
	}

	if err := runCommand(dotfilesDir, "git", "add", "."); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	message, err := readCommitMessage()
	if err != nil {
		return fmt.Errorf("read commit message: %w", err)
	}
	if message == "" {
		return errors.New("commit message cannot be empty")
	}

	if err := runCommand(dotfilesDir, "git", "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	if err := runCommand(dotfilesDir, "git", "push"); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	return nil
}

func parseMap(mapPath, dotfilesDir string) ([]mapping, error) {
	f, err := os.Open(mapPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open map file %s: %w", mapPath, err)
	}
	defer f.Close()

	var mappings []mapping
	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		repoPart, systemPart, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("invalid mapping at %s:%d", mapPath, lineNumber)
		}

		repoRel, err := sanitizeRepoPath(repoPart)
		if err != nil {
			return nil, fmt.Errorf("invalid repo path at %s:%d: %w", mapPath, lineNumber, err)
		}

		systemRaw := strings.TrimSpace(systemPart)
		systemAbs, err := expandPath(systemRaw, dotfilesDir)
		if err != nil {
			return nil, fmt.Errorf("invalid system path at %s:%d: %w", mapPath, lineNumber, err)
		}

		mappings = append(mappings, mapping{
			RepoRel:   repoRel,
			SystemRaw: systemRaw,
			SystemAbs: systemAbs,
			Line:      lineNumber,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read map file %s: %w", mapPath, err)
	}

	return mappings, nil
}

func appendMapping(mapPath, repoRel, systemPath string) error {
	if err := os.MkdirAll(filepath.Dir(mapPath), 0o755); err != nil {
		return fmt.Errorf("create map directory: %w", err)
	}

	needsNewline := false
	content, err := os.ReadFile(mapPath)
	if err == nil {
		if len(content) > 0 && content[len(content)-1] != '\n' {
			needsNewline = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read map file before append: %w", err)
	}

	f, err := os.OpenFile(mapPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open map file for append: %w", err)
	}
	defer f.Close()

	if needsNewline {
		if _, err := f.WriteString("\n"); err != nil {
			return fmt.Errorf("write map newline: %w", err)
		}
	}

	if _, err := f.WriteString(fmt.Sprintf("%s : %s\n", repoRel, systemPath)); err != nil {
		return fmt.Errorf("append map entry: %w", err)
	}

	return nil
}

func mappingStatus(dotfilesDir string, m mapping) (string, error) {
	repoAbs := repoAbsPath(dotfilesDir, m.RepoRel)
	ignoreMatcher, err := ignoreMatcherForExistingPath(repoAbs)
	if err != nil {
		return "", err
	}
	if ignoreMatcher != nil {
		return mappingStatusWithIgnore(repoAbs, m.SystemAbs, ignoreMatcher)
	}

	info, err := os.Lstat(m.SystemAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "MISSING", nil
		}
		return "", err
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return "STRAY", nil
	}

	pointsToRepo, err := symlinkPointsTo(m.SystemAbs, repoAbs)
	if err != nil {
		return "", err
	}

	if !pointsToRepo {
		return "STRAY", nil
	}

	if _, err := os.Stat(repoAbs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "BROKEN", nil
		}
		return "", err
	}

	return "OK", nil
}

func repoAbsPath(dotfilesDir, repoRel string) string {
	return filepath.Join(dotfilesDir, filepath.FromSlash(repoRel))
}

func symlinkPointsTo(linkPath, expectedTarget string) (bool, error) {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return false, err
	}

	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(linkPath), target)
	}

	target = filepath.Clean(target)
	expectedTarget = filepath.Clean(expectedTarget)

	return target == expectedTarget, nil
}

func linkTrackedPath(repoAbs, systemAbs string, ignoreMatcher *ignoreMatcher, simulate bool) error {
	if ignoreMatcher != nil {
		_, err := linkIgnoredDirectory(repoAbs, systemAbs, ignoreMatcher, simulate)
		return err
	}

	if err := os.Symlink(repoAbs, systemAbs); err != nil {
		return err
	}

	return nil
}

func linkMapping(dotfilesDir string, m mapping, simulate bool) (int, error) {
	repoAbs := repoAbsPath(dotfilesDir, m.RepoRel)
	ignoreMatcher, err := ignoreMatcherForExistingPath(repoAbs)
	if err != nil {
		return 0, fmt.Errorf("load ignore file for %s: %w", repoAbs, err)
	}
	if ignoreMatcher != nil {
		return linkIgnoredDirectory(repoAbs, m.SystemAbs, ignoreMatcher, simulate)
	}

	info, err := os.Lstat(m.SystemAbs)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			pointsToRepo, readErr := symlinkPointsTo(m.SystemAbs, repoAbs)
			if readErr == nil && pointsToRepo {
				return 0, nil
			}
		}

		warnConflict(m.SystemAbs, simulate)
		return 1, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("inspect %s: %w", m.SystemAbs, err)
	}

	if simulate {
		parentDir := filepath.Dir(m.SystemAbs)
		if info, err := os.Stat(parentDir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Printf("Simulate: would create directory %s\n", parentDir)
			} else {
				return 0, fmt.Errorf("inspect parent directory for %s: %w", m.SystemAbs, err)
			}
		} else if !info.IsDir() {
			return 0, fmt.Errorf("parent directory path is not a directory: %s", parentDir)
		}

		fmt.Printf("Simulate: would link %s -> %s\n", m.SystemAbs, repoAbs)
		return 0, nil
	}

	if err := os.MkdirAll(filepath.Dir(m.SystemAbs), 0o755); err != nil {
		return 0, fmt.Errorf("create parent directory for %s: %w", m.SystemAbs, err)
	}

	if err := os.Symlink(repoAbs, m.SystemAbs); err != nil {
		return 0, fmt.Errorf("create symlink %s: %w", m.SystemAbs, err)
	}

	fmt.Printf("Linked %s -> %s\n", m.SystemAbs, repoAbs)
	return 0, nil
}

func linkIgnoredDirectory(repoDir, systemDir string, ignoreMatcher *ignoreMatcher, simulate bool) (int, error) {
	conflicts, ready, err := ensureManagedDirectory(repoDir, systemDir, simulate)
	if err != nil || !ready {
		return conflicts, err
	}

	childConflicts, err := linkIgnoredDirectoryContents(repoDir, systemDir, "", ignoreMatcher, simulate)
	if err != nil {
		return conflicts, err
	}

	if !simulate {
		fmt.Printf("Linked %s -> %s (ignoring matched entries)\n", systemDir, repoDir)
	}

	return conflicts + childConflicts, nil
}

func ensureManagedDirectory(repoDir, systemDir string, simulate bool) (int, bool, error) {
	info, err := os.Lstat(systemDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return 0, false, fmt.Errorf("inspect %s: %w", systemDir, err)
		}

		if simulate {
			fmt.Printf("Simulate: would create directory %s\n", systemDir)
			return 0, true, nil
		}

		if err := os.MkdirAll(systemDir, 0o755); err != nil {
			return 0, false, fmt.Errorf("create directory %s: %w", systemDir, err)
		}

		return 0, true, nil
	}

	if info.IsDir() {
		return 0, true, nil
	}

	if info.Mode()&os.ModeSymlink != 0 {
		pointsToRepo, readErr := symlinkPointsTo(systemDir, repoDir)
		if readErr == nil && pointsToRepo {
			if simulate {
				fmt.Printf("Simulate: would replace symlink %s with directory\n", systemDir)
				return 0, true, nil
			}

			if err := os.Remove(systemDir); err != nil {
				return 0, false, fmt.Errorf("remove symlink %s: %w", systemDir, err)
			}

			if err := os.Mkdir(systemDir, 0o755); err != nil {
				return 0, false, fmt.Errorf("create directory %s: %w", systemDir, err)
			}

			return 0, true, nil
		}
	}

	warnConflict(systemDir, simulate)
	return 1, false, nil
}

func linkIgnoredDirectoryContents(repoDir, systemDir, rel string, ignoreMatcher *ignoreMatcher, simulate bool) (int, error) {
	sourceDir := repoDir
	if rel != "" {
		sourceDir = filepath.Join(repoDir, rel)
	}

	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return 0, fmt.Errorf("read directory %s: %w", sourceDir, err)
	}

	conflicts := 0
	for _, entry := range entries {
		childRel := entry.Name()
		if rel != "" {
			childRel = filepath.Join(rel, entry.Name())
		}

		if ignoreMatcher.Match(childRel) {
			continue
		}

		repoPath := filepath.Join(repoDir, childRel)
		systemPath := filepath.Join(systemDir, childRel)
		info, err := entry.Info()
		if err != nil {
			return conflicts, fmt.Errorf("inspect %s: %w", repoPath, err)
		}

		if info.IsDir() {
			dirConflicts, ready, err := ensureManagedDirectory(repoPath, systemPath, simulate)
			if err != nil {
				return conflicts, err
			}
			conflicts += dirConflicts
			if !ready {
				continue
			}

			childConflicts, err := linkIgnoredDirectoryContents(repoDir, systemDir, childRel, ignoreMatcher, simulate)
			if err != nil {
				return conflicts, err
			}
			conflicts += childConflicts
			continue
		}

		fileConflicts, err := ensureManagedSymlink(repoPath, systemPath, simulate)
		if err != nil {
			return conflicts, err
		}
		conflicts += fileConflicts
	}

	return conflicts, nil
}

func ensureManagedSymlink(repoPath, systemPath string, simulate bool) (int, error) {
	info, err := os.Lstat(systemPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			pointsToRepo, readErr := symlinkPointsTo(systemPath, repoPath)
			if readErr == nil && pointsToRepo {
				return 0, nil
			}
		}

		warnConflict(systemPath, simulate)
		return 1, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("inspect %s: %w", systemPath, err)
	}

	if simulate {
		fmt.Printf("Simulate: would link %s -> %s\n", systemPath, repoPath)
		return 0, nil
	}

	if err := os.Symlink(repoPath, systemPath); err != nil {
		return 0, fmt.Errorf("create symlink %s: %w", systemPath, err)
	}

	return 0, nil
}

func simulateIgnoredDirectoryLayout(repoDir, systemDir string, ignoreMatcher *ignoreMatcher) error {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", repoDir, err)
	}

	for _, entry := range entries {
		rel := entry.Name()
		if ignoreMatcher.Match(rel) {
			continue
		}

		if err := simulateIgnoredPath(repoDir, systemDir, rel, ignoreMatcher); err != nil {
			return err
		}
	}

	return nil
}

func simulateIgnoredPath(repoDir, systemDir, rel string, ignoreMatcher *ignoreMatcher) error {
	repoPath := filepath.Join(repoDir, rel)
	systemPath := filepath.Join(systemDir, rel)
	info, err := os.Lstat(repoPath)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", repoPath, err)
	}

	if info.IsDir() {
		fmt.Printf("Simulate: would create directory %s\n", systemPath)
		entries, err := os.ReadDir(repoPath)
		if err != nil {
			return fmt.Errorf("read directory %s: %w", repoPath, err)
		}

		for _, entry := range entries {
			childRel := filepath.Join(rel, entry.Name())
			if ignoreMatcher.Match(childRel) {
				continue
			}
			if err := simulateIgnoredPath(repoDir, systemDir, childRel, ignoreMatcher); err != nil {
				return err
			}
		}

		return nil
	}

	fmt.Printf("Simulate: would link %s -> %s\n", systemPath, repoPath)
	return nil
}

func warnConflict(path string, simulate bool) {
	if simulate {
		fmt.Fprintf(os.Stderr, "Simulate: conflict at %s (manual resolution required)\n", path)
		return
	}

	fmt.Fprintf(os.Stderr, "Warning: conflict at %s (manual resolution required)\n", path)
}

func ignoreMatcherForDir(path string, info os.FileInfo) (*ignoreMatcher, error) {
	if info == nil || !info.IsDir() {
		return nil, nil
	}

	return loadIgnoreMatcher(path)
}

func ignoreMatcherForExistingPath(path string) (*ignoreMatcher, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil
	}

	return loadIgnoreMatcher(path)
}

func loadIgnoreMatcher(packageDir string) (*ignoreMatcher, error) {
	matcher := &ignoreMatcher{}
	loaded := false

	for _, name := range []string{localIgnoreFileName, compatIgnoreFileName} {
		ignorePath := filepath.Join(packageDir, name)
		f, err := os.Open(ignorePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("open ignore file %s: %w", ignorePath, err)
		}

		loaded = true
		scanner := bufio.NewScanner(f)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			pattern := stripIgnoreComment(scanner.Text())
			if pattern == "" {
				continue
			}

			re, err := regexp.Compile(pattern)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("invalid ignore pattern at %s:%d: %w", ignorePath, lineNumber, err)
			}

			if strings.Contains(pattern, "/") {
				matcher.path = append(matcher.path, re)
			} else {
				matcher.basename = append(matcher.basename, re)
			}
		}

		if err := scanner.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("read ignore file %s: %w", ignorePath, err)
		}

		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close ignore file %s: %w", ignorePath, err)
		}
	}

	if !loaded {
		return nil, nil
	}

	return matcher, nil
}

func stripIgnoreComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] != '#' {
			continue
		}

		backslashes := 0
		for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
			backslashes++
		}

		if backslashes%2 == 0 {
			return strings.TrimSpace(line[:i])
		}
	}

	return strings.TrimSpace(line)
}

func (m *ignoreMatcher) Match(rel string) bool {
	if m == nil {
		return false
	}

	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" {
		return false
	}

	base := path.Base(rel)
	if base == localIgnoreFileName || base == compatIgnoreFileName {
		return true
	}

	for _, re := range m.path {
		for _, candidate := range ignorePathCandidates(rel) {
			if matchesFullRegexp(re, candidate) {
				return true
			}
		}
	}

	for _, re := range m.basename {
		if matchesFullRegexp(re, base) {
			return true
		}
	}

	return false
}

func ignorePathCandidates(rel string) []string {
	parts := strings.Split(rel, "/")
	candidates := make([]string, 0, len(parts)*2)
	for i := 0; i < len(parts); i++ {
		suffix := strings.Join(parts[i:], "/")
		candidates = append(candidates, "/"+suffix, suffix)
	}
	return candidates
}

func matchesFullRegexp(re *regexp.Regexp, value string) bool {
	loc := re.FindStringIndex(value)
	return loc != nil && loc[0] == 0 && loc[1] == len(value)
}

func mappingStatusWithIgnore(repoDir, systemDir string, ignoreMatcher *ignoreMatcher) (string, error) {
	if _, err := os.Stat(repoDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "BROKEN", nil
		}
		return "", err
	}

	info, err := os.Lstat(systemDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "MISSING", nil
		}
		return "", err
	}

	if !info.IsDir() {
		return "STRAY", nil
	}

	return ignoredDirectoryStatus(repoDir, systemDir, "", ignoreMatcher)
}

func ignoredDirectoryStatus(repoDir, systemDir, rel string, ignoreMatcher *ignoreMatcher) (string, error) {
	sourceDir := repoDir
	if rel != "" {
		sourceDir = filepath.Join(repoDir, rel)
	}

	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "BROKEN", nil
		}
		return "", err
	}

	for _, entry := range entries {
		childRel := entry.Name()
		if rel != "" {
			childRel = filepath.Join(rel, entry.Name())
		}

		if ignoreMatcher.Match(childRel) {
			continue
		}

		repoPath := filepath.Join(repoDir, childRel)
		systemPath := filepath.Join(systemDir, childRel)
		info, err := entry.Info()
		if err != nil {
			return "", err
		}

		targetInfo, err := os.Lstat(systemPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "MISSING", nil
			}
			return "", err
		}

		if info.IsDir() {
			if !targetInfo.IsDir() {
				return "STRAY", nil
			}

			status, err := ignoredDirectoryStatus(repoDir, systemDir, childRel, ignoreMatcher)
			if err != nil || status != "OK" {
				return status, err
			}
			continue
		}

		if targetInfo.Mode()&os.ModeSymlink == 0 {
			return "STRAY", nil
		}

		pointsToRepo, err := symlinkPointsTo(systemPath, repoPath)
		if err != nil {
			return "", err
		}
		if !pointsToRepo {
			return "STRAY", nil
		}

		if _, err := os.Stat(repoPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "BROKEN", nil
			}
			return "", err
		}
	}

	return "OK", nil
}

func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}

		if err := os.Symlink(target, dst); err != nil {
			return err
		}

		return os.Remove(src)
	}

	if info.IsDir() {
		if err := copyDir(src, dst, info.Mode().Perm()); err != nil {
			_ = os.RemoveAll(dst)
			return err
		}

		return os.RemoveAll(src)
	}

	if info.Mode().IsRegular() {
		if err := copyFile(src, dst, info.Mode().Perm()); err != nil {
			_ = os.Remove(dst)
			return err
		}

		return os.Remove(src)
	}

	return fmt.Errorf("unsupported file type: %s", src)
}

func copyDir(src, dst string, perm os.FileMode) error {
	if err := os.Mkdir(dst, perm); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		targetPath := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		mode := info.Mode()
		switch {
		case mode.IsDir():
			return os.Mkdir(targetPath, mode.Perm())
		case mode&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, targetPath)
		case mode.IsRegular():
			return copyFile(path, targetPath, mode.Perm())
		default:
			return fmt.Errorf("unsupported file type inside directory: %s", path)
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	return nil
}

func runCommand(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func readCommitMessage() (string, error) {
	fmt.Print("commit message: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func resolveDotfilesDir() (string, error) {
	raw := os.Getenv("DOTFILES")
	if strings.TrimSpace(raw) == "" {
		raw = "~/.dotfiles"
	}

	return expandPath(raw, "")
}

func expandPath(rawPath, dotfilesDir string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", errors.New("empty path")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	} else if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("unsupported home expansion form: %s", rawPath)
	}

	path = os.Expand(path, func(key string) string {
		if key == "DOTFILES" && dotfilesDir != "" {
			return dotfilesDir
		}
		return os.Getenv(key)
	})

	if !filepath.IsAbs(path) {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = absPath
	}

	return filepath.Clean(path), nil
}

func sanitizeRepoPath(rawPath string) (string, error) {
	clean := strings.TrimSpace(rawPath)
	if clean == "" {
		return "", errors.New("path cannot be empty")
	}

	clean = filepath.ToSlash(filepath.Clean(filepath.FromSlash(clean)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path must be inside DOTFILES: %s", rawPath)
	}

	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("path must be relative: %s", rawPath)
	}

	return clean, nil
}

func compressHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	path = filepath.Clean(path)
	home = filepath.Clean(home)

	if path == home {
		return "~"
	}

	prefix := home + string(os.PathSeparator)
	if strings.HasPrefix(path, prefix) {
		return "~" + path[len(home):]
	}

	return path
}

func printUsage() {
	fmt.Println("dot - minimalist dotfile manager")
	fmt.Println("")
	fmt.Println("usage:")
	fmt.Println("  dot [--simulate|-n] track <file> [target_path]")
	fmt.Println("  dot [--simulate|-n] link")
	fmt.Println("  dot [--simulate|-n] list")
	fmt.Println("  dot [--simulate|-n] sync")
	fmt.Println("")
	fmt.Println("Options:")
	fmt.Println("  -n, --simulate  Dry run; print actions without making changes")
	fmt.Println("")
	fmt.Println("Environment:")
	fmt.Println("  DOTFILES  Repository path (default: ~/.dotfiles)")
}
