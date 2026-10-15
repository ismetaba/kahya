package shell

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// errGitAlternates is gitRepoRoots' fail-closed answer for a repository
// whose object store borrows another one's objects (objects/info/
// alternates): `git show` there can print blobs of a repository this
// package never resolved, so the caller treats it as a secret-lane path hit.
var errGitAlternates = errors.New("shell_host: repository uses an alternate object store")

// gitRepoRoots returns, without running git, every directory that names
// the repository `git -C dir` would open (FIX-FINAL shell-host-subdir-or-
// gitdir-bypasses-secret-repo-path-hit). The secret-lane path check used
// to look at the -C string only, so `-C ~/Kahya/memory/inbox`,
// `-C ~/Kahya/backups` or `-C ~/Kahya/.git` opened the ~/Kahya repository
// (whose history holds memory/gizli/**) as a path miss. git's own discovery
// walks up from dir: at each level it first takes dir/.git (a gitdir, or a
// "gitdir:" file), then dir itself when dir is a gitdir (a bare repo, or a
// directory inside .git), and it stops at the first match. This walk
// mirrors that order and, for the match, returns:
//   - the directory holding a .git entry (the work-tree root);
//   - the gitdir itself and, when it is named .git, its parent;
//   - the common dir (a linked worktree's commondir) and, when it is named
//     .git, its parent; and the same for the real directory behind a
//     symlinked objects store;
//   - for every path component of dir named .git, its parent (over-
//     inclusive on purpose).
//
// It is exec-free, so it runs on the receipt-replay path too, before any
// policy decision. Any read error other than "does not exist" is returned
// (the caller treats an error as a path hit: fail-closed), and so is a
// repository with an alternate object store (errGitAlternates). The walk
// deliberately does not continue past the first repository: a dotfile repo
// at ~ would otherwise make every git call under ~ a hit.
func gitRepoRoots(dir string) ([]string, error) {
	dir = filepath.Clean(dir)
	var roots []string
	for p := dir; ; {
		if strings.EqualFold(filepath.Base(p), ".git") {
			roots = append(roots, filepath.Dir(p))
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	for a := dir; ; {
		dotgit := filepath.Join(a, ".git")
		fi, err := os.Stat(dotgit) // git follows a symlinked .git
		switch {
		case err == nil && fi.Mode().IsRegular():
			gd, gerr := readGitfile(dotgit)
			if gerr != nil {
				return roots, gerr
			}
			r, rerr := gitDirRoots(gd)
			return append(append(roots, a), r...), rerr
		case err == nil && fi.IsDir():
			ok, verr := isGitDir(dotgit)
			if verr != nil {
				return roots, verr
			}
			if ok {
				r, rerr := gitDirRoots(dotgit)
				return append(append(roots, a), r...), rerr
			}
		case err != nil && !notExist(err):
			return roots, err
		}
		ok, verr := isGitDir(a)
		if verr != nil {
			return roots, verr
		}
		if ok {
			r, rerr := gitDirRoots(a)
			return append(roots, r...), rerr
		}
		parent := filepath.Dir(a)
		if parent == a {
			return roots, nil
		}
		a = parent
	}
}

// notExist is true for the errors git's discovery treats as "nothing here"
// (a missing entry, or a path component that is a file).
func notExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// readGitfile parses a "gitdir: <path>" file the way git does (a relative
// path is relative to the file's directory) and returns the resolved gitdir.
func readGitfile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	gd, ok := strings.CutPrefix(strings.TrimRight(line, " \t\r"), "gitdir: ")
	if !ok || gd == "" {
		return "", fmt.Errorf("shell_host: malformed gitfile %s", path)
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(filepath.Dir(path), gd)
	}
	return filepath.EvalSymlinks(gd)
}

// isGitDir mirrors git's is_git_directory, erring STRICT (a directory git
// would reject is never reported as one, so the walk never stops below the
// repository git really opens): HEAD is a symlink into refs/, or a file
// starting "ref: refs/" or with a hex object id; the common dir holds
// objects/ and refs/ directories.
func isGitDir(d string) (bool, error) {
	head := filepath.Join(d, "HEAD")
	fi, err := os.Lstat(head)
	if err != nil {
		if notExist(err) {
			return false, nil
		}
		return false, err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(head)
		if err != nil {
			return false, err
		}
		if !strings.HasPrefix(target, "refs/") {
			return false, nil
		}
	case fi.Mode().IsRegular():
		b, err := os.ReadFile(head)
		if err != nil {
			return false, err
		}
		if !validHeadContent(string(b)) {
			return false, nil
		}
	default:
		return false, nil
	}
	common, err := gitCommonDir(d)
	if err != nil {
		return false, err
	}
	for _, sub := range []string{"objects", "refs"} {
		si, err := os.Stat(filepath.Join(common, sub))
		if err != nil {
			if notExist(err) {
				return false, nil
			}
			return false, err
		}
		if !si.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

// validHeadContent is git's validate_headref for a regular HEAD file.
func validHeadContent(s string) bool {
	if strings.HasPrefix(s, "ref: refs/") {
		return true
	}
	n := 0
	for n < len(s) && strings.ContainsRune("0123456789abcdef", rune(s[n])) {
		n++
	}
	return n == 40 || n == 64
}

// gitCommonDir is git's get_common_dir: the "commondir" file's target
// (relative to d unless absolute), else d itself.
func gitCommonDir(d string) (string, error) {
	b, err := os.ReadFile(filepath.Join(d, "commondir"))
	if err != nil {
		if notExist(err) {
			return d, nil
		}
		return "", err
	}
	c := strings.TrimRight(string(b), "\r\n \t")
	if c == "" {
		return d, nil
	}
	if !filepath.IsAbs(c) {
		c = filepath.Join(d, c)
	}
	return filepath.Clean(c), nil
}

// gitDirRoots is the set of directories naming gitdir g's repository (see
// gitRepoRoots), or errGitAlternates.
func gitDirRoots(g string) ([]string, error) {
	real, err := filepath.EvalSymlinks(g)
	if err != nil {
		return nil, err
	}
	roots := withDotGitParent(nil, real)
	common, err := gitCommonDir(real)
	if err != nil {
		return roots, err
	}
	if rc, err := filepath.EvalSymlinks(common); err == nil {
		common = rc
	} else if !notExist(err) {
		return roots, err
	}
	roots = withDotGitParent(roots, common)
	objects := filepath.Join(common, "objects")
	if ro, err := filepath.EvalSymlinks(objects); err == nil {
		roots = withDotGitParent(roots, filepath.Dir(ro))
		objects = ro
	} else if !notExist(err) {
		return roots, err
	}
	for _, alt := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(objects, "info", alt)); err == nil {
			return roots, errGitAlternates
		} else if !notExist(err) {
			return roots, err
		}
	}
	return roots, nil
}

// withDotGitParent appends g and, when g is named .git, its parent.
func withDotGitParent(roots []string, g string) []string {
	roots = append(roots, g)
	if strings.EqualFold(filepath.Base(g), ".git") {
		roots = append(roots, filepath.Dir(g))
	}
	return roots
}
