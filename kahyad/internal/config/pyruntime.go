package config

// pyruntime.go: B-4 review (round 2) — the code a Python child kahyad spawns
// (worker, embed, Qwen) executes OUTSIDE its venv and its import root: the
// BASE interpreter's install prefix (the stdlib every start-up imports —
// encodings, site, json …, lib-dynload's extension modules, the framework /
// libpython dylib) and the non-system libraries those native images link
// (Homebrew's _ssl links <prefix>/opt/openssl@3/lib/libssl.3.dylib).
// Under Homebrew all of that is user-owned, and fs_write has no write-root
// allowlist, only deny globs, so without these entries a W1 fs_write (auto
// at L2) could replace encodings/__init__.py or libssl and run code on the
// host at the next spawn. SelfProtectDenyGlobs adds what this file derives.

import (
	"bufio"
	"bytes"
	"debug/macho"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Mach-O load commands that name a dylib dyld loads with the image.
// debug/macho decodes only LC_LOAD_DYLIB into *macho.Dylib; the others are
// read from the raw load-command bytes (same dylib_command layout).
const (
	lcLoadDylib       = 0xc
	lcLoadWeakDylib   = 0x18 | 0x80000000
	lcReexportDylib   = 0x1f | 0x80000000
	lcLazyLoadDylib   = 0x20
	lcLoadUpwardDylib = 0x23 | 0x80000000

	// maxNativeImagesScanned bounds the dependency walk (a Homebrew Python
	// has ~80 native images: the interpreter, its framework dylib and
	// lib-dynload/*.so).
	maxNativeImagesScanned = 1024
)

// pythonRuntimeProtect returns the paths a Python child started as argv0
// executes outside its venv and import root: subtrees (each gets "/**")
// and exact files.
//
//   - argv0 is resolved the way exec.Command resolves it at spawn time: a
//     bare name through kahyad's own PATH (exec.LookPath); a relative path
//     with a slash yields nothing (it could never match a canonical path).
//   - The base interpreter: argv0 itself, its symlink-resolved target (a
//     venv's bin/python is a symlink into the base install), and — for a
//     venv, including a --copies one — pyvenv.cfg's "home" (directory of
//     the base executable) and "executable" keys.
//   - Its install prefix: <dir of the interpreter's bin>/.. (sys.base_prefix
//     for every standard layout: Homebrew/python.org framework
//     Versions/X.Y, pyenv, uv, /usr/local), which holds lib/pythonX.Y,
//     lib-dynload, pythonXY.zip and the framework dylib. A prefix that is
//     "/" or contains the user's home directory would deny every ordinary
//     write, so only its lib/ subtree (where the stdlib lives) is denied.
//   - A prefix inside a Homebrew keg (<hb>/Cellar/<formula>/<version>, with
//     <hb>/bin/brew present) denies the whole Homebrew prefix <hb>: every
//     keg the stdlib's extension modules link lives there (openssl@3, xz,
//     zstd, sqlite, mpdecimal), each reached through <hb>/opt symlinks, and
//     so does the config those libraries load code from at init
//     (<hb>/etc/openssl@3/openssl.cnf can name a provider module).
//   - The dylibs the interpreter, its framework/libpython dylib and every
//     lib-dynload module link (LC_LOAD/WEAK/REEXPORT/LAZY/UPWARD_DYLIB),
//     followed transitively: an absolute dependency outside the SIP-
//     protected system trees is denied exactly (even when absent — a
//     missing weak dependency can be planted), or its whole Homebrew prefix
//     when it lives in a Homebrew keg. @rpath/@loader_path/@executable_path
//     names resolve relative to the loading image, i.e. inside the prefix
//     already denied, and are not chased. A non-Mach-O image (Linux ELF,
//     a shell-script shim) contributes no dependencies.
//
// Only an interpreter named python* (python, python3, python3.14, pythonw;
// its own name or its symlink target's) gets a prefix and a scan — the
// "<bin>/.." layout is a Python convention; any other argv0 (a test's fake
// worker script) is protected as its exact file only.
func pythonRuntimeProtect(argv0 string) (subtrees, files []string) {
	exe := resolveCommandPath(argv0)
	if exe == "" {
		return nil, nil
	}
	if !looksLikePythonInterpreter(exe) {
		files = append(files, exe)
		if r := evalSymlinksOrEmpty(exe); r != "" && r != exe {
			files = append(files, r)
		}
		return nil, files
	}
	homes := homeForms()
	addSubtree := func(p string) {
		if p != "" && !slices.Contains(subtrees, p) {
			subtrees = append(subtrees, p)
		}
	}
	addFile := func(p string) {
		if p != "" && !slices.Contains(files, p) {
			files = append(files, p)
		}
	}
	var interps, prefixes []string
	addInterp := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		for _, form := range []string{filepath.Clean(p), evalSymlinksOrEmpty(p)} {
			if form != "" && !slices.Contains(interps, form) {
				interps = append(interps, form)
			}
		}
	}
	addPrefix := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		for _, form := range []string{filepath.Clean(p), evalSymlinksOrEmpty(p)} {
			if form != "" && !slices.Contains(prefixes, form) {
				prefixes = append(prefixes, form)
			}
		}
	}

	addInterp(exe)
	// pyvenv.cfg sits in the venv root (the parent of bin/) or next to the
	// executable; Python looks it up from the UNresolved executable path.
	for _, cfgPath := range []string{filepath.Join(filepath.Dir(filepath.Dir(exe)), "pyvenv.cfg"), filepath.Join(filepath.Dir(exe), "pyvenv.cfg")} {
		home, executable := readPyvenvCfg(cfgPath)
		if home != "" && filepath.IsAbs(home) {
			addPrefix(filepath.Dir(filepath.Clean(home)))
			if r := evalSymlinksOrEmpty(home); r != "" {
				addPrefix(filepath.Dir(r))
			}
		}
		addInterp(executable)
	}
	for _, i := range interps {
		addFile(i)
		addPrefix(filepath.Dir(filepath.Dir(i)))
	}

	for _, p := range prefixes {
		if isBroadPrefix(p, homes) {
			addSubtree(filepath.Join(p, "lib"))
		} else {
			addSubtree(p)
		}
		if hb := homebrewPrefix(p); hb != "" && !isBroadPrefix(hb, homes) {
			addSubtree(hb)
		}
	}

	// Native images the interpreter loads from its prefix.
	queue := append([]string{}, interps...)
	for _, p := range prefixes {
		queue = append(queue, filepath.Join(p, "Python"))
		for _, pattern := range []string{
			filepath.Join(p, "lib", "libpython*.dylib"),
			filepath.Join(p, "lib", "python*", "lib-dynload", "*.so"),
		} {
			if m, err := filepath.Glob(pattern); err == nil {
				queue = append(queue, m...)
			}
		}
	}
	scanned := make(map[string]bool)
	for len(queue) > 0 && len(scanned) < maxNativeImagesScanned {
		img := queue[0]
		queue = queue[1:]
		if scanned[img] {
			continue
		}
		scanned[img] = true
		for _, dep := range machoDylibDeps(img) {
			if !filepath.IsAbs(dep) || isSIPProtectedPath(dep) {
				continue
			}
			dep = filepath.Clean(dep)
			resolved := evalSymlinksOrEmpty(dep)
			hb := homebrewPrefix(dep)
			if hb == "" && resolved != "" {
				hb = homebrewPrefix(resolved)
			}
			if hb != "" && !isBroadPrefix(hb, homes) {
				addSubtree(hb)
			} else {
				addFile(dep)
			}
			if resolved != "" {
				queue = append(queue, resolved)
			}
		}
	}
	return subtrees, files
}

// looksLikePythonInterpreter reports whether exe (or its symlink target) is
// named python*.
func looksLikePythonInterpreter(exe string) bool {
	for _, p := range []string{exe, evalSymlinksOrEmpty(exe)} {
		if p != "" && strings.HasPrefix(strings.ToLower(filepath.Base(p)), "python") {
			return true
		}
	}
	return false
}

// resolveCommandPath turns a command's argv0 into the absolute path
// exec.Command will run: an absolute path as is, a bare name through PATH,
// "" otherwise (relative path with a slash, not found, or "." in PATH).
func resolveCommandPath(argv0 string) string {
	if argv0 == "" {
		return ""
	}
	if filepath.IsAbs(argv0) {
		return filepath.Clean(argv0)
	}
	if strings.ContainsRune(argv0, filepath.Separator) {
		return ""
	}
	p, err := exec.LookPath(argv0)
	if err != nil || !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

// readPyvenvCfg returns pyvenv.cfg's "home" and "executable" values ("" when
// the file or key is absent). Format: "key = value" lines.
func readPyvenvCfg(path string) (home, executable string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "home":
			home = strings.TrimSpace(v)
		case "executable":
			executable = strings.TrimSpace(v)
		}
	}
	return home, executable
}

// homebrewPrefix returns the Homebrew prefix <hb> when p lies inside its
// Cellar (<hb>/Cellar/...) and <hb>/bin/brew exists; "" otherwise. Only
// the nearest "Cellar" ancestor counts.
func homebrewPrefix(p string) string {
	for a := filepath.Clean(p); ; a = filepath.Dir(a) {
		if filepath.Base(a) == "Cellar" {
			hb := filepath.Dir(a)
			if fi, err := os.Stat(filepath.Join(hb, "bin", "brew")); err == nil && !fi.IsDir() {
				return hb
			}
			return ""
		}
		if filepath.Dir(a) == a {
			return ""
		}
	}
}

// isBroadPrefix reports whether denying p's whole subtree would deny every
// ordinary write: p is "/" or the user's home directory or one of its
// ancestors (case-insensitive, like MatchesAnyGlobCI).
func isBroadPrefix(p string, homes []string) bool {
	lp := strings.ToLower(filepath.Clean(p))
	if lp == string(filepath.Separator) {
		return true
	}
	for _, h := range homes {
		lh := strings.ToLower(h)
		if lh == lp || strings.HasPrefix(lh, lp+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// homeForms returns the user's home directory as configured and symlink-
// resolved (both absolute); empty when unknown.
func homeForms() []string {
	h, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(h) {
		return nil
	}
	out := []string{filepath.Clean(h)}
	if r := evalSymlinksOrEmpty(h); r != "" && r != out[0] {
		out = append(out, r)
	}
	return out
}

// isSIPProtectedPath reports whether p is in a macOS System Integrity
// Protection tree no user process can write (/System, /usr except
// /usr/local, /bin, /sbin). Such a dependency needs no deny glob.
func isSIPProtectedPath(p string) bool {
	for _, root := range []string{"/System/", "/bin/", "/sbin/"} {
		if strings.HasPrefix(p, root) {
			return true
		}
	}
	return strings.HasPrefix(p, "/usr/") && !strings.HasPrefix(p, "/usr/local/")
}

func evalSymlinksOrEmpty(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return r
}

// machoDylibDeps returns the install names of every dylib the Mach-O image
// at path (thin or universal) loads; nil for anything else.
func machoDylibDeps(path string) []string {
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		return machoLoadNames(f)
	}
	ff, err := macho.OpenFat(path)
	if err != nil {
		return nil
	}
	defer ff.Close()
	var out []string
	for _, a := range ff.Arches {
		for _, n := range machoLoadNames(a.File) {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

func machoLoadNames(f *macho.File) []string {
	var out []string
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) < 12 {
			continue
		}
		switch f.ByteOrder.Uint32(raw[0:4]) {
		case lcLoadDylib, lcLoadWeakDylib, lcReexportDylib, lcLazyLoadDylib, lcLoadUpwardDylib:
		default:
			continue
		}
		off := f.ByteOrder.Uint32(raw[8:12])
		if off < 12 || int(off) >= len(raw) {
			continue
		}
		name := raw[off:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		if len(name) > 0 {
			out = append(out, string(name))
		}
	}
	return out
}
