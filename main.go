package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	mapFileName          = ".dot.map"
	localIgnoreFileName  = ".dot-local-ignore"
	compatIgnoreFileName = ".stow-local-ignore"
)

var errWalkDone = errors.New("walk done")

type mapping struct {
	RepoRel   string
	SystemRaw string
	SystemAbs string
	Line      int
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
	if len(args) == 0 {
		printUsage()
		return nil
	}

	dotfilesDir, err := resolveDotfilesDir()
	if err != nil {
		return fmt.Errorf("resolve DOTFILES: %w", err)
	}

	switch args[0] {
	case "track":
		return cmdTrack(dotfilesDir, args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	}

	cmds := map[string]func(string) error{"link": cmdLink, "list": cmdList, "sync": cmdSync}
	cmd, ok := cmds[args[0]]
	if !ok {
		printUsage()
		return fmt.Errorf("unknown command: %s", args[0])
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: dot %s", args[0])
	}
	return cmd(dotfilesDir)
}

func cmdTrack(dotfilesDir string, args []string) error {
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

	repoArg, repoLabel := filepath.Base(sourceAbs), "source file name"
	if len(args) == 2 {
		repoArg, repoLabel = args[1], "target_path"
	}
	repoRel, err := sanitizeRepoPath(repoArg)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", repoLabel, err)
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

	if err := os.MkdirAll(filepath.Dir(repoAbs), 0o755); err != nil {
		return fmt.Errorf("create target_path directory: %w", err)
	}

	if err := movePath(sourceAbs, repoAbs); err != nil {
		return fmt.Errorf("move source into DOTFILES: %w", err)
	}

	if _, err := os.Stat(repoAbs); err != nil {
		return rollbackTrack(repoAbs, sourceAbs, err, "move verification failed", nil)
	}

	ignoreMatcher, err := ignoreMatcherForPath(repoAbs, info)
	if err != nil {
		return rollbackTrack(repoAbs, sourceAbs, err, "load ignore file failed", nil)
	}

	if ignoreMatcher == nil {
		err = os.Symlink(repoAbs, sourceAbs)
	} else {
		_, err = linkIgnoredDirectory(repoAbs, sourceAbs, ignoreMatcher)
	}
	if err != nil {
		return rollbackTrack(repoAbs, sourceAbs, err, "create symlink failed", func() error { return os.RemoveAll(sourceAbs) })
	}

	if err := appendMapping(mapPath, repoRel, compressHome(sourceAbs)); err != nil {
		return rollbackTrack(repoAbs, sourceAbs, err, "write map failed", func() error { return os.RemoveAll(sourceAbs) })
	}

	fmt.Printf("Tracked %s -> %s\n", sourceAbs, repoRel)
	return nil
}

func cmdLink(dotfilesDir string) error {
	return withMappings(dotfilesDir, func(mappings []mapping) error {
		conflicts := 0
		for _, m := range mappings {
			mappingConflicts, err := linkMapping(dotfilesDir, m)
			if err != nil {
				return err
			}
			conflicts += mappingConflicts
		}
		if conflicts > 0 {
			fmt.Fprintf(os.Stderr, "Skipped %d conflict(s).\n", conflicts)
		}
		return nil
	})
}

func cmdList(dotfilesDir string) error {
	return withMappings(dotfilesDir, func(mappings []mapping) error {
		for _, m := range mappings {
			status, err := mappingStatus(dotfilesDir, m)
			if err != nil {
				return fmt.Errorf("status for line %d: %w", m.Line, err)
			}
			fmt.Printf("%-8s %s : %s\n", status, m.RepoRel, m.SystemRaw)
		}
		return nil
	})
}

func cmdSync(dotfilesDir string) error {
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

func withMappings(dotfilesDir string, fn func([]mapping) error) error {
	mapPath := filepath.Join(dotfilesDir, mapFileName)
	mappings, err := parseMap(mapPath, dotfilesDir)
	if err != nil || len(mappings) > 0 {
		if err != nil {
			return err
		}
		return fn(mappings)
	}
	fmt.Printf("No mappings found in %s\n", mapPath)
	return nil
}

func parseMap(mapPath, dotfilesDir string) ([]mapping, error) {
	var mappings []mapping
	_, err := visitFileLines(mapPath, "map file", true, func(lineNumber int, line string) error {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			return nil
		}

		repoPart, systemPart, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("invalid mapping at %s:%d", mapPath, lineNumber)
		}

		repoRel, err := sanitizeRepoPath(repoPart)
		if err != nil {
			return fmt.Errorf("invalid repo path at %s:%d: %w", mapPath, lineNumber, err)
		}

		systemRaw := strings.TrimSpace(systemPart)
		systemAbs, err := expandPath(systemRaw, dotfilesDir)
		if err != nil {
			return fmt.Errorf("invalid system path at %s:%d: %w", mapPath, lineNumber, err)
		}

		mappings = append(mappings, mapping{
			RepoRel:   repoRel,
			SystemRaw: systemRaw,
			SystemAbs: systemAbs,
			Line:      lineNumber,
		})
		return nil
	})
	return mappings, err
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
	ignoreMatcher, err := ignoreMatcherForPath(repoAbs, nil)
	if err != nil {
		return "", err
	}
	if ignoreMatcher != nil {
		return mappingStatusWithIgnore(repoAbs, m.SystemAbs, ignoreMatcher)
	}
	_, status, err := inspectLink(m.SystemAbs, repoAbs)
	return status, err
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

func linkMapping(dotfilesDir string, m mapping) (int, error) {
	repoAbs := repoAbsPath(dotfilesDir, m.RepoRel)
	ignoreMatcher, err := ignoreMatcherForPath(repoAbs, nil)
	if err != nil {
		return 0, fmt.Errorf("load ignore file for %s: %w", repoAbs, err)
	}
	if ignoreMatcher != nil {
		return linkIgnoredDirectory(repoAbs, m.SystemAbs, ignoreMatcher)
	}
	exists, status, err := inspectLink(m.SystemAbs, repoAbs)
	if err != nil {
		return 0, fmt.Errorf("inspect %s: %w", m.SystemAbs, err)
	}
	if status == "OK" {
		return 0, nil
	}
	if exists {
		warnConflict(m.SystemAbs)
		return 1, nil
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

func linkIgnoredDirectory(repoDir, systemDir string, ignoreMatcher *ignoreMatcher) (int, error) {
	conflicts, ready, err := ensureManagedPath(repoDir, systemDir, true)
	if err != nil || !ready {
		return conflicts, err
	}

	err = walkManagedPaths(repoDir, systemDir, ignoreMatcher, func(repoPath, systemPath string, d fs.DirEntry) error {
		childConflicts, ready, err := ensureManagedPath(repoPath, systemPath, d.IsDir())
		conflicts += childConflicts
		if err != nil || !d.IsDir() || ready {
			return err
		}
		return fs.SkipDir
	})
	if err != nil {
		return conflicts, err
	}

	fmt.Printf("Linked %s -> %s (ignoring matched entries)\n", systemDir, repoDir)

	return conflicts, nil
}

func ensureManagedPath(repoPath, systemPath string, dir bool) (int, bool, error) {
	if !dir {
		exists, status, err := inspectLink(systemPath, repoPath)
		if err != nil {
			return 0, false, fmt.Errorf("inspect %s: %w", systemPath, err)
		}
		if status == "OK" {
			return 0, true, nil
		}
		if !exists {
			if err := os.Symlink(repoPath, systemPath); err != nil {
				return 0, false, fmt.Errorf("create symlink %s: %w", systemPath, err)
			}
			return 0, true, nil
		}
		warnConflict(systemPath)
		return 1, false, nil
	}

	info, err := os.Lstat(systemPath)
	switch {
	case err == nil:
		if info.IsDir() {
			return 0, true, nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			pointsToRepo, readErr := symlinkPointsTo(systemPath, repoPath)
			if readErr == nil && pointsToRepo {
				if err := os.Remove(systemPath); err != nil {
					return 0, false, fmt.Errorf("remove symlink %s: %w", systemPath, err)
				}
				if err := os.MkdirAll(systemPath, 0o755); err != nil {
					return 0, false, fmt.Errorf("create directory %s: %w", systemPath, err)
				}
				return 0, true, nil
			}
		}
		warnConflict(systemPath)
		return 1, false, nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(systemPath, 0o755); err != nil {
			return 0, false, fmt.Errorf("create directory %s: %w", systemPath, err)
		}
		return 0, true, nil
	default:
		return 0, false, fmt.Errorf("inspect %s: %w", systemPath, err)
	}
}

func warnConflict(path string) {
	fmt.Fprintf(os.Stderr, "Warning: conflict at %s (manual resolution required)\n", path)
}

func ignoreMatcherForPath(path string, info os.FileInfo) (*ignoreMatcher, error) {
	if info == nil {
		var err error
		info, err = os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
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
		found, err := visitFileLines(ignorePath, "ignore file", true, func(lineNumber int, line string) error {
			pattern := stripIgnoreComment(line)
			if pattern == "" {
				return nil
			}

			re, err := regexp.Compile("^(?:" + pattern + ")$")
			if err != nil {
				return fmt.Errorf("invalid ignore pattern at %s:%d: %w", ignorePath, lineNumber, err)
			}

			if strings.Contains(pattern, "/") {
				matcher.path = append(matcher.path, re)
			} else {
				matcher.basename = append(matcher.basename, re)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		loaded = loaded || found
	}

	if !loaded {
		return nil, nil
	}

	return matcher, nil
}

func visitFileLines(path, label string, allowMissing bool, fn func(int, string) error) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open %s %s: %w", label, path, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if err := fn(i+1, line); err != nil {
			return true, err
		}
	}
	return true, nil
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
		parts := strings.Split(rel, "/")
		for i := range parts {
			suffix := strings.Join(parts[i:], "/")
			if re.MatchString(suffix) || re.MatchString("/"+suffix) {
				return true
			}
		}
	}

	for _, re := range m.basename {
		if re.MatchString(base) {
			return true
		}
	}

	return false
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

	status := "OK"
	err = walkManagedPaths(repoDir, systemDir, ignoreMatcher, func(repoPath, systemPath string, d fs.DirEntry) error {
		info, err := os.Lstat(systemPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				status = "MISSING"
				return errWalkDone
			}
			return err
		}

		if d.IsDir() {
			if !info.IsDir() {
				status = "STRAY"
				return errWalkDone
			}
			return nil
		}

		_, status, err = inspectLink(systemPath, repoPath)
		if err != nil || status != "OK" {
			return errOrDone(err, status)
		}
		return nil
	})
	if err != nil && !errors.Is(err, errWalkDone) {
		return "", err
	}
	return status, nil
}

func walkManagedPaths(repoDir, systemDir string, ignoreMatcher *ignoreMatcher, visit func(repoPath, systemPath string, d fs.DirEntry) error) error {
	return filepath.WalkDir(repoDir, func(repoPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(repoDir, repoPath)
		if err != nil || rel == "." {
			return err
		}
		if ignoreMatcher.Match(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return visit(repoPath, filepath.Join(systemDir, rel), d)
	})
}

func inspectLink(systemPath, repoPath string) (bool, string, error) {
	info, err := os.Lstat(systemPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "MISSING", nil
		}
		return false, "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true, "STRAY", nil
	}
	ok, err := symlinkPointsTo(systemPath, repoPath)
	if err != nil {
		return true, "", err
	}
	if !ok {
		return true, "STRAY", nil
	}
	if _, err := os.Stat(repoPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, "BROKEN", nil
		}
		return true, "", err
	}
	return true, "OK", nil
}

func errOrDone(err error, status string) error {
	if err != nil {
		return err
	}
	if status != "OK" {
		return errWalkDone
	}
	return nil
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
	if err := copyPath(src, dst, info); err != nil {
		if info.IsDir() {
			_ = os.RemoveAll(dst)
		} else {
			_ = os.Remove(dst)
		}
		return err
	}
	if info.IsDir() {
		return os.RemoveAll(src)
	}
	return os.Remove(src)
}

func rollbackTrack(repoAbs, sourceAbs string, cause error, action string, cleanup func() error) error {
	var cleanupErr error
	if cleanup != nil {
		cleanupErr = cleanup()
	}
	moveErr := movePath(repoAbs, sourceAbs)
	if cleanupErr != nil || moveErr != nil {
		return fmt.Errorf("%s (%v), cleanup failed (%v), rollback failed (%v)", action, cause, cleanupErr, moveErr)
	}
	return fmt.Errorf("%s: %w", action, cause)
}

func copyPath(src, dst string, info os.FileInfo) error {
	switch mode := info.Mode(); {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case mode.IsDir():
		if err := os.Mkdir(dst, mode.Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			name := entry.Name()
			if err := copyPath(filepath.Join(src, name), filepath.Join(dst, name), info); err != nil {
				return err
			}
		}
		return nil
	case mode.IsRegular():
		return copyFile(src, dst, mode.Perm())
	default:
		return fmt.Errorf("unsupported file type: %s", src)
	}
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
	raw := strings.TrimSpace(os.Getenv("DOTFILES"))
	if raw == "" {
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
	fmt.Print(`dot - minimalist dotfile manager

usage:
  dot track <file> [target_path]
  dot link
  dot list
  dot sync

Environment:
  DOTFILES  Repository path (default: ~/.dotfiles)
`)
}
