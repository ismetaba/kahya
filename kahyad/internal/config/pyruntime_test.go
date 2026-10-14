package config

// pyruntime_test.go: B-4 review round 2 — SelfProtectDenyGlobs covers the
// code a Python child runs from its BASE interpreter (outside the venv and
// the import root): the install prefix's stdlib / lib-dynload / framework
// dylib, the dylibs those native images link, and a Homebrew prefix.
// Every fixture is a synthesized tree under t.TempDir(); the Mach-O images
// are minimal thin arm64 headers carrying only dylib load commands.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	mcpfs "kahya/mcp/fs"
)

type fakeDylibLoad struct {
	cmd  uint32
	name string
}

// writeFakeMachO writes a thin little-endian arm64 Mach-O image whose only
// load commands are the given dylib_commands.
func writeFakeMachO(t *testing.T, path string, loads ...fakeDylibLoad) {
	t.Helper()
	var cmds []byte
	for _, l := range loads {
		name := append([]byte(l.name), 0)
		size := 24 + len(name)
		if pad := size % 8; pad != 0 {
			size += 8 - pad
		}
		c := make([]byte, size)
		binary.LittleEndian.PutUint32(c[0:], l.cmd)
		binary.LittleEndian.PutUint32(c[4:], uint32(size))
		binary.LittleEndian.PutUint32(c[8:], 24) // name offset
		binary.LittleEndian.PutUint32(c[12:], 2) // timestamp
		binary.LittleEndian.PutUint32(c[16:], 0x10000)
		binary.LittleEndian.PutUint32(c[20:], 0x10000)
		copy(c[24:], name)
		cmds = append(cmds, c...)
	}
	h := make([]byte, 32)
	binary.LittleEndian.PutUint32(h[0:], 0xfeedfacf) // MH_MAGIC_64
	binary.LittleEndian.PutUint32(h[4:], 0x0100000c) // CPU_TYPE_ARM64
	binary.LittleEndian.PutUint32(h[12:], 8)         // MH_BUNDLE
	binary.LittleEndian.PutUint32(h[16:], uint32(len(loads)))
	binary.LittleEndian.PutUint32(h[20:], uint32(len(cmds)))
	mustWriteFile(t, path, append(h, cmds...))
	// Executable like a real interpreter/dylib (exec.LookPath needs +x).
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// canonicalDenied checks p the way mcp/fs.HandleWrite does: canonicalize
// (symlinks of the deepest existing ancestor resolved, NFC) and match.
func canonicalDenied(t *testing.T, globs []string, home, p string) bool {
	t.Helper()
	cp, err := mcpfs.Canonicalize(home, p)
	if err != nil {
		t.Fatalf("Canonicalize(%s): %v", p, err)
	}
	return selfProtectHit(t, globs, cp.Match)
}

// fakeBasePython lays out a standard base-interpreter prefix (framework
// Versions/X.Y shape) whose _ssl extension links ext/lib/libssl.3.dylib
// (which links libcrypto) plus an absent weak dependency, and returns the
// prefix and the interpreter path.
func fakeBasePython(t *testing.T, root, ext string) (prefix, interp string) {
	t.Helper()
	prefix = filepath.Join(root, "Python.framework", "Versions", "3.14")
	interp = filepath.Join(prefix, "bin", "python3.14")
	writeFakeMachO(t, interp,
		fakeDylibLoad{lcLoadDylib, filepath.Join(prefix, "Python")},
		fakeDylibLoad{lcLoadDylib, "/usr/lib/libSystem.B.dylib"})
	writeFakeMachO(t, filepath.Join(prefix, "Python"), fakeDylibLoad{lcLoadDylib, "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"})
	mustWriteFile(t, filepath.Join(prefix, "lib", "python3.14", "encodings", "__init__.py"), []byte("# stdlib\n"))
	writeFakeMachO(t, filepath.Join(prefix, "lib", "python3.14", "lib-dynload", "_ssl.cpython-314-darwin.so"),
		fakeDylibLoad{lcLoadDylib, filepath.Join(ext, "lib", "libssl.3.dylib")},
		fakeDylibLoad{lcLoadWeakDylib, filepath.Join(ext, "lib", "libmissing.dylib")},
		fakeDylibLoad{lcLoadDylib, "@loader_path/../libinside.dylib"})
	writeFakeMachO(t, filepath.Join(ext, "lib", "libssl.3.dylib"), fakeDylibLoad{lcReexportDylib, filepath.Join(ext, "lib", "libcrypto.3.dylib")})
	writeFakeMachO(t, filepath.Join(ext, "lib", "libcrypto.3.dylib"))
	mustWriteFile(t, filepath.Join(ext, "lib", "README.txt"), []byte("not linked\n"))
	return prefix, interp
}

// TestSelfProtectDenyGlobsCoverBaseInterpreterPrefix (B-4 review round 2):
// the worker venv's python is a symlink into a base install whose stdlib
// is user-writable (Homebrew). encodings/__init__.py, json/__init__.py,
// lib-dynload and the framework dylib — imported/loaded by every start-up
// — and the dylibs the stdlib's extension modules link must be
// write-protected, so a W1 fs_write cannot run code at the next spawn.
func TestSelfProtectDenyGlobsCoverBaseInterpreterPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	ext := filepath.Join(root, "ext")
	prefix, interp := fakeBasePython(t, filepath.Join(root, "base"), ext)

	venvPy := filepath.Join(root, "proj", "worker", ".venv", "bin", "python")
	mustSymlink(t, interp, venvPy)
	mustWriteFile(t, filepath.Join(root, "proj", "worker", ".venv", "pyvenv.cfg"), []byte("home = "+filepath.Join(prefix, "bin")+"\ninclude-system-site-packages = false\n"))

	globs := Config{WorkerCmd: []string{venvPy, "-m", "kahya_worker"}}.SelfProtectDenyGlobs()
	for _, p := range []string{
		filepath.Join(prefix, "lib", "python3.14", "encodings", "__init__.py"),
		filepath.Join(prefix, "lib", "python3.14", "json", "__init__.py"),
		filepath.Join(prefix, "lib", "python3.14", "lib-dynload", "_ssl.cpython-314-darwin.so"),
		filepath.Join(prefix, "lib", "python314.zip"),
		filepath.Join(prefix, "Python"),
		interp,
		filepath.Join(ext, "lib", "libssl.3.dylib"),
		filepath.Join(ext, "lib", "libcrypto.3.dylib"),
		filepath.Join(ext, "lib", "libmissing.dylib"),
	} {
		if !canonicalDenied(t, globs, home, p) {
			t.Errorf("%s (run by every worker spawn through the base interpreter) must be write-protected; globs %v", p, globs)
		}
	}
	for _, p := range []string{filepath.Join(ext, "lib", "README.txt"), filepath.Join(ext, "notes.py"), filepath.Join(root, "proj", "docs", "x.md"), filepath.Join(home, "notes.txt")} {
		if canonicalDenied(t, globs, home, p) {
			t.Errorf("%s is not run by the interpreter and must stay writable; globs %v", p, globs)
		}
	}
	for _, g := range globs {
		if !filepath.IsAbs(g) || strings.HasPrefix(g, "/usr/lib/") || strings.HasPrefix(g, "/System/") {
			t.Errorf("unexpected glob %q (relative, or a SIP-protected system dependency)", g)
		}
	}
}

// TestSelfProtectDenyGlobsCoverBaseInterpreterOfCopiedVenvAndBareInterpreter:
// a --copies venv (bin/python is a real file, so only pyvenv.cfg names the
// base) and a bare interpreter outside any venv both protect the base
// install's stdlib — not just the interpreter file.
func TestSelfProtectDenyGlobsCoverBaseInterpreterOfCopiedVenvAndBareInterpreter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	prefix, interp := fakeBasePython(t, filepath.Join(root, "base"), filepath.Join(root, "ext"))
	stdlib := filepath.Join(prefix, "lib", "python3.14", "encodings", "__init__.py")

	copied := filepath.Join(root, "copies", ".venv", "bin", "python3")
	writeFakeMachO(t, copied)
	for name, cfgText := range map[string]string{
		"home":       "home = " + filepath.Join(prefix, "bin") + "\n",
		"executable": "version = 3.14.6\nexecutable = " + interp + "\n",
	} {
		t.Run("copies venv, pyvenv.cfg "+name, func(t *testing.T) {
			mustWriteFile(t, filepath.Join(root, "copies", ".venv", "pyvenv.cfg"), []byte(cfgText))
			globs := Config{EmbedCmd: []string{copied, filepath.Join(root, "copies", "server.py")}}.SelfProtectDenyGlobs()
			if !canonicalDenied(t, globs, home, stdlib) {
				t.Errorf("base stdlib %s must be protected for a --copies venv; globs %v", stdlib, globs)
			}
		})
	}
	t.Run("bare interpreter", func(t *testing.T) {
		globs := Config{QwenCmd: []string{interp, "-m", "mlx_lm.server"}}.SelfProtectDenyGlobs()
		for _, p := range []string{stdlib, filepath.Join(prefix, "lib", "python3.14", "lib-dynload", "_ssl.cpython-314-darwin.so"), interp} {
			if !canonicalDenied(t, globs, home, p) {
				t.Errorf("bare interpreter: %s must be protected; globs %v", p, globs)
			}
		}
	})
	t.Run("bare name resolved through PATH", func(t *testing.T) {
		bin := t.TempDir()
		mustSymlink(t, interp, filepath.Join(bin, "python3"))
		t.Setenv("PATH", bin)
		globs := Config{EmbedCmd: []string{"python3", filepath.Join(root, "svc", "server.py")}}.SelfProtectDenyGlobs()
		if !canonicalDenied(t, globs, home, stdlib) {
			t.Errorf("a bare python3 (exec.Command resolves it via PATH) must protect its base stdlib; globs %v", globs)
		}
	})
}

// fakeHomebrew lays out <hb>/Cellar/python@3.14/3.14.6 (framework shape)
// with <hb>/opt/python@3.14 -> ../Cellar/python@3.14/3.14.6, as Homebrew
// installs it, and returns hb and the opt-path interpreter.
func fakeHomebrew(t *testing.T, hb string, marker bool) (optInterp string) {
	t.Helper()
	keg := filepath.Join(hb, "Cellar", "python@3.14", "3.14.6")
	versions := filepath.Join(keg, "Frameworks", "Python.framework", "Versions", "3.14")
	writeFakeMachO(t, filepath.Join(versions, "bin", "python3.14"),
		fakeDylibLoad{lcLoadDylib, filepath.Join(hb, "opt", "python@3.14", "Frameworks", "Python.framework", "Versions", "3.14", "Python")})
	writeFakeMachO(t, filepath.Join(versions, "Python"))
	mustWriteFile(t, filepath.Join(versions, "lib", "python3.14", "encodings", "__init__.py"), []byte("# stdlib\n"))
	mustSymlink(t, "../Frameworks/Python.framework/Versions/3.14/bin/python3.14", filepath.Join(keg, "bin", "python3.14"))
	mustSymlink(t, "../Cellar/python@3.14/3.14.6", filepath.Join(hb, "opt", "python@3.14"))
	if marker {
		mustWriteFile(t, filepath.Join(hb, "bin", "brew"), []byte("#!/bin/sh\n"))
	}
	return filepath.Join(hb, "opt", "python@3.14", "bin", "python3.14")
}

// TestSelfProtectDenyGlobsCoverHomebrewPrefix: under Homebrew the stdlib's
// extension modules link other kegs (openssl@3, xz, zstd, sqlite,
// mpdecimal) through <hb>/opt, and OpenSSL loads provider modules named in
// <hb>/etc/openssl@3/openssl.cnf at init — all user-writable. A venv
// whose interpreter resolves into a Homebrew Cellar denies the whole
// Homebrew prefix; a "Cellar" directory without bin/brew does not.
func TestSelfProtectDenyGlobsCoverHomebrewPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	hb := filepath.Join(root, "homebrew")
	optInterp := fakeHomebrew(t, hb, true)
	venvPy := filepath.Join(root, "proj", "worker", ".venv", "bin", "python")
	mustSymlink(t, optInterp, venvPy)
	mustWriteFile(t, filepath.Join(root, "proj", "worker", ".venv", "pyvenv.cfg"), []byte("home = "+filepath.Join(hb, "opt", "python@3.14", "bin")+"\n"))

	globs := Config{WorkerCmd: []string{venvPy, "-m", "kahya_worker"}}.SelfProtectDenyGlobs()
	if !slices.Contains(globs, filepath.Join(mustEvalSymlinks(t, hb), "**")) {
		t.Errorf("the Homebrew prefix %s/** must be denied; globs %v", hb, globs)
	}
	for _, p := range []string{
		filepath.Join(hb, "Cellar", "python@3.14", "3.14.6", "Frameworks", "Python.framework", "Versions", "3.14", "lib", "python3.14", "encodings", "__init__.py"),
		filepath.Join(hb, "opt", "python@3.14", "Frameworks", "Python.framework", "Versions", "3.14", "lib", "python3.14", "json", "__init__.py"),
		filepath.Join(hb, "Cellar", "openssl@3", "3.6.0", "lib", "libssl.3.dylib"),
		filepath.Join(hb, "opt", "openssl@3", "lib", "libssl.3.dylib"),
		filepath.Join(hb, "etc", "openssl@3", "openssl.cnf"),
	} {
		if !canonicalDenied(t, globs, home, p) {
			t.Errorf("Homebrew path %s must be write-protected; globs %v", p, globs)
		}
	}

	// A Cellar-shaped tree without Homebrew's bin/brew: only the
	// interpreter's own prefix is denied, not its parent tree.
	other := filepath.Join(root, "notbrew")
	optOther := fakeHomebrew(t, other, false)
	g2 := Config{EmbedCmd: []string{optOther, filepath.Join(root, "svc", "server.py")}}.SelfProtectDenyGlobs()
	if canonicalDenied(t, g2, home, filepath.Join(other, "etc", "x.conf")) {
		t.Errorf("without bin/brew the tree above Cellar must stay writable; globs %v", g2)
	}
	if !canonicalDenied(t, g2, home, filepath.Join(other, "Cellar", "python@3.14", "3.14.6", "Frameworks", "Python.framework", "Versions", "3.14", "lib", "python3.14", "os.py")) {
		t.Errorf("the interpreter's own prefix must still be protected; globs %v", g2)
	}
}

// TestSelfProtectDenyGlobsCoverHomebrewKegLinkedFromNonHomebrewPython: a
// pyenv-style interpreter outside Homebrew whose _ssl links Homebrew's
// openssl@3 (through <hb>/opt) gets the Homebrew prefix denied from the
// dependency alone.
func TestSelfProtectDenyGlobsCoverHomebrewKegLinkedFromNonHomebrewPython(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	hb := filepath.Join(root, "homebrew")
	mustWriteFile(t, filepath.Join(hb, "bin", "brew"), []byte("#!/bin/sh\n"))
	writeFakeMachO(t, filepath.Join(hb, "Cellar", "openssl@3", "3.6.0", "lib", "libssl.3.dylib"))
	mustSymlink(t, "../Cellar/openssl@3/3.6.0", filepath.Join(hb, "opt", "openssl@3"))

	prefix := filepath.Join(root, "pyenv", "versions", "3.14.6")
	interp := filepath.Join(prefix, "bin", "python3.14")
	writeFakeMachO(t, interp)
	writeFakeMachO(t, filepath.Join(prefix, "lib", "python3.14", "lib-dynload", "_ssl.cpython-314-darwin.so"),
		fakeDylibLoad{lcLoadDylib, filepath.Join(hb, "opt", "openssl@3", "lib", "libssl.3.dylib")})

	globs := Config{WorkerCmd: []string{interp, "-m", "kahya_worker"}}.SelfProtectDenyGlobs()
	for _, p := range []string{filepath.Join(hb, "etc", "openssl@3", "openssl.cnf"), filepath.Join(hb, "Cellar", "openssl@3", "3.6.0", "lib", "libcrypto.3.dylib"), filepath.Join(prefix, "lib", "python3.14", "site.py")} {
		if !canonicalDenied(t, globs, home, p) {
			t.Errorf("%s must be write-protected; globs %v", p, globs)
		}
	}
}

// TestSelfProtectDenyGlobsNeverDenyHomeForBroadInterpreterPrefix: an
// interpreter at ~/bin/python3 has prefix ~ — denying ~/** would refuse
// every ordinary write, so only ~/lib/** (its stdlib) and the interpreter
// are denied. A non-Python argv0 (a test's fake worker) is protected as
// its exact file only, never its parent tree.
func TestSelfProtectDenyGlobsNeverDenyHomeForBroadInterpreterPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	interp := filepath.Join(home, "bin", "python3")
	writeFakeMachO(t, interp)
	globs := Config{WorkerCmd: []string{interp, filepath.Join(home, "svc", "worker.py")}}.SelfProtectDenyGlobs()
	for _, g := range globs {
		if g == filepath.Join(home, "**") || g == filepath.Join(mustEvalSymlinks(t, home), "**") || g == "/**" {
			t.Errorf("glob %q denies the whole home directory", g)
		}
	}
	if !canonicalDenied(t, globs, home, filepath.Join(home, "lib", "python3.14", "os.py")) || !canonicalDenied(t, globs, home, interp) {
		t.Errorf("~/lib/** and the interpreter must be protected; globs %v", globs)
	}
	if canonicalDenied(t, globs, home, filepath.Join(home, "notes", "a.md")) {
		t.Errorf("an ordinary home file must stay writable; globs %v", globs)
	}

	root := t.TempDir()
	fake := filepath.Join(root, "x", "fake-worker.sh")
	mustWriteFile(t, fake, []byte("#!/bin/sh\n"))
	g2 := Config{WorkerCmd: []string{fake}}.SelfProtectDenyGlobs()
	if !canonicalDenied(t, g2, home, fake) {
		t.Errorf("the fake worker file itself must be protected; globs %v", g2)
	}
	if canonicalDenied(t, g2, home, filepath.Join(root, "x", "other.txt")) || canonicalDenied(t, g2, home, filepath.Join(root, "lib", "y.py")) {
		t.Errorf("a non-Python argv0 must not deny its parent tree; globs %v", g2)
	}
}

// TestMachoDylibDepsReadsRealUniversalBinary: the dependency reader parses
// a real (universal) system binary, not only the synthesized thin images.
func TestMachoDylibDepsReadsRealUniversalBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Mach-O system binaries exist on darwin only")
	}
	deps := machoDylibDeps("/usr/bin/true")
	if !slices.Contains(deps, "/usr/lib/libSystem.B.dylib") {
		t.Fatalf("machoDylibDeps(/usr/bin/true) = %v, want /usr/lib/libSystem.B.dylib", deps)
	}
	if got := machoDylibDeps("/etc/hosts"); got != nil {
		t.Fatalf("a non-Mach-O file must yield no dependencies, got %v", got)
	}
}
