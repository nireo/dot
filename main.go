package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const mapFileName = ".dot.map"

type mapping struct {
	RepoRel   string
	SystemRaw string
	SystemAbs string
	Line      int
}

type globalOptions struct {
	Simulate bool
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
		fmt.Printf("Simulate: would create symlink %s -> %s\n", sourceAbs, repoAbs)
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

	if err := os.Symlink(repoAbs, sourceAbs); err != nil {
		rollbackErr := movePath(repoAbs, sourceAbs)
		if rollbackErr != nil {
			return fmt.Errorf("create symlink failed (%v) and rollback failed (%v)", err, rollbackErr)
		}
		return fmt.Errorf("create symlink: %w", err)
	}

	if err := appendMapping(mapPath, repoRel, compressHome(sourceAbs)); err != nil {
		removeErr := os.Remove(sourceAbs)
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
		repoAbs := repoAbsPath(dotfilesDir, m.RepoRel)

		info, err := os.Lstat(m.SystemAbs)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				pointsToRepo, readErr := symlinkPointsTo(m.SystemAbs, repoAbs)
				if readErr == nil && pointsToRepo {
					continue
				}
			}

			conflicts++
			if simulate {
				fmt.Fprintf(os.Stderr, "Simulate: conflict at %s (manual resolution required)\n", m.SystemAbs)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: conflict at %s (manual resolution required)\n", m.SystemAbs)
			}
			continue
		}

		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", m.SystemAbs, err)
		}

		if simulate {
			parentDir := filepath.Dir(m.SystemAbs)
			if info, err := os.Stat(parentDir); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					fmt.Printf("Simulate: would create directory %s\n", parentDir)
				} else {
					return fmt.Errorf("inspect parent directory for %s: %w", m.SystemAbs, err)
				}
			} else if !info.IsDir() {
				return fmt.Errorf("parent directory path is not a directory: %s", parentDir)
			}

			fmt.Printf("Simulate: would link %s -> %s\n", m.SystemAbs, repoAbs)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(m.SystemAbs), 0o755); err != nil {
			return fmt.Errorf("create parent directory for %s: %w", m.SystemAbs, err)
		}

		if err := os.Symlink(repoAbs, m.SystemAbs); err != nil {
			return fmt.Errorf("create symlink %s: %w", m.SystemAbs, err)
		}

		fmt.Printf("Linked %s -> %s\n", m.SystemAbs, repoAbs)
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
