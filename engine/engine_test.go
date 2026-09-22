package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeInfo is a minimal os.FileInfo for remote entries.
type fakeInfo struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) ModTime() time.Time { return f.mtime }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }
func (f fakeInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func TestNeedsUpdate(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	remote := fakeInfo{size: 10, mtime: base}
	cases := []struct {
		name  string
		local fakeInfo
		want  bool
	}{
		{"same", fakeInfo{size: 10, mtime: base}, false},
		{"sub-second mtime diff", fakeInfo{size: 10, mtime: base.Add(500 * time.Millisecond)}, false},
		{"size differs", fakeInfo{size: 11, mtime: base}, true},
		{"mtime differs", fakeInfo{size: 10, mtime: base.Add(time.Second)}, true},
	}
	for _, c := range cases {
		if got := needsUpdate(c.local, remote); got != c.want {
			t.Errorf("%s: needsUpdate = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLocalRel(t *testing.T) {
	const root = "/share/modpacks/Valheim"
	ok := map[string]string{
		root + "/BepInEx/plugins/a.dll": "BepInEx/plugins/a.dll",
		root + "/~mods/x y.pak":         "~mods/x y.pak",
	}
	for in, want := range ok {
		relSlash, rel, good := localRel(root, in)
		if !good || relSlash != want || rel != filepath.FromSlash(want) {
			t.Errorf("localRel(%q) = %q, %q, %v; want %q", in, relSlash, rel, good, want)
		}
	}
	bad := []string{
		root,                         // the root itself
		"/share/modpacks/Valheim2/x", // sibling dir sharing the prefix
		"/elsewhere/a.dll",
		root + "/../escape.dll",
		root + "/a/../../escape.dll",
		root + "//a.dll",
		root + "/a/",
	}
	if runtime.GOOS == "windows" {
		bad = append(bad,
			root+`/..\..\escape.dll`,
			root+"/C:/Windows/evil.dll",
			root+"/a.dll:stream",
			root+"/NUL",
		)
	}
	for _, in := range bad {
		if relSlash, rel, good := localRel(root, in); good {
			t.Errorf("localRel(%q) accepted as %q / %q, want rejected", in, relSlash, rel)
		}
	}
}

func TestValidCleanupPath(t *testing.T) {
	for _, p := range []string{"BepInEx/plugins", "R5/Content/Paks/~mods/~mods"} {
		if !validCleanupPath(p) {
			t.Errorf("validCleanupPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", ".", "..", "../x", "a/../b", "/abs", "a/", "a//b", `a\b`, "C:/x"} {
		if validCleanupPath(p) {
			t.Errorf("validCleanupPath(%q) = true, want false", p)
		}
	}
}

// mkTree creates files (and their parent dirs) under root; names ending in "/"
// are created as directories.
func mkTree(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(root, filepath.FromSlash(n))
		if n[len(n)-1] == '/' {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// entries builds a remote listing; names ending in "/" are directories.
func entries(names ...string) []sftpEntry {
	var out []sftpEntry
	for _, n := range names {
		dir := n[len(n)-1] == '/'
		rel := n
		if dir {
			rel = n[:len(n)-1]
		}
		out = append(out, sftpEntry{RelSlash: rel, Info: fakeInfo{name: filepath.Base(rel), dir: dir}})
	}
	return out
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

func TestMirrorFiles(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"BepInEx/plugins/keep.dll",
		"BepInEx/plugins/Case.dll", // remote has "case.dll"
		"BepInEx/plugins/orphan.dll",
		"BepInEx/plugins/sub/keep.cfg",
		"BepInEx/plugins/sub/orphan.cfg",
		"BepInEx/plugins/oldmod/a.dll",
		"BepInEx/config/local.cfg", // outside the cleanup path
		"outside.txt",
	)
	remote := entries(
		"BepInEx/", "BepInEx/plugins/",
		"BepInEx/plugins/keep.dll",
		"BepInEx/plugins/case.dll",
		"BepInEx/plugins/sub/",
		"BepInEx/plugins/sub/keep.cfg",
	)
	runCleanup(root, []CleanupSpec{{Path: "BepInEx/plugins", Mode: MirrorFiles}}, remote)

	for _, rel := range []string{
		"BepInEx/plugins/keep.dll", "BepInEx/plugins/Case.dll", "BepInEx/plugins/sub/keep.cfg",
		"BepInEx/config/local.cfg", "outside.txt",
	} {
		if !exists(root, rel) {
			t.Errorf("%s was deleted, want kept", rel)
		}
	}
	for _, rel := range []string{
		"BepInEx/plugins/orphan.dll", "BepInEx/plugins/sub/orphan.cfg", "BepInEx/plugins/oldmod",
	} {
		if exists(root, rel) {
			t.Errorf("%s survived, want deleted", rel)
		}
	}
}

func TestMirrorSubdirs(t *testing.T) {
	root := t.TempDir()
	const mods = "R5/Binaries/Win64/ue4ss/Mods"
	mkTree(t, root,
		mods+"/mods.txt",        // file at mirror level: never deleted
		mods+"/Keep/main.lua",   // remote has "keep": same dir on NTFS
		mods+"/Keep/local.lua",  // inside a kept subdir: untouched
		mods+"/Orphan/main.lua", // subdir gone on SFTP
		"R5/Binaries/Win64/game.exe",
	)
	remote := entries(mods+"/", mods+"/keep/", mods+"/keep/main.lua")
	runCleanup(root, []CleanupSpec{{Path: mods, Mode: MirrorSubdirs}}, remote)

	for _, rel := range []string{mods + "/mods.txt", mods + "/Keep/main.lua", mods + "/Keep/local.lua", "R5/Binaries/Win64/game.exe"} {
		if !exists(root, rel) {
			t.Errorf("%s was deleted, want kept", rel)
		}
	}
	if exists(root, mods+"/Orphan") {
		t.Errorf("orphan subdir survived, want deleted")
	}
}

func TestMirrorMissingDirIsNoop(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root, "outside.txt")
	runCleanup(root, []CleanupSpec{
		{Path: "BepInEx/plugins", Mode: MirrorFiles},
		{Path: "R5/Mods", Mode: MirrorSubdirs},
	}, entries("other/"))
	if !exists(root, "outside.txt") {
		t.Fatal("file outside cleanup paths was deleted")
	}
}

func TestResumeMeta(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "a.dll.part")
	info := fakeInfo{size: 100, mtime: time.Unix(1_700_000_000, 0)}

	if metaMatches(part, info) {
		t.Fatal("metaMatches with no meta = true")
	}
	if err := writeMeta(part, info); err != nil {
		t.Fatal(err)
	}
	if !metaMatches(part, info) {
		t.Fatal("metaMatches after writeMeta = false")
	}
	if metaMatches(part, fakeInfo{size: 101, mtime: info.mtime}) {
		t.Fatal("metaMatches ignored a size change")
	}
	if metaMatches(part, fakeInfo{size: 100, mtime: info.mtime.Add(time.Second)}) {
		t.Fatal("metaMatches ignored an mtime change")
	}

	if got := partOffset(part, 100); got != 0 {
		t.Fatalf("partOffset(missing) = %d, want 0", got)
	}
	if err := os.WriteFile(part, make([]byte, 40), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := partOffset(part, 100); got != 40 {
		t.Fatalf("partOffset = %d, want 40", got)
	}
	if got := partOffset(part, 30); got != 0 {
		t.Fatalf("partOffset(oversized part) = %d, want 0", got)
	}
}

func TestFinalize(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "a.dll")
	part := local + ".part"
	mtime := time.Unix(1_700_000_000, 0)
	mkTree(t, dir, "a.dll", "a.dll.part", "a.dll.part.meta") // old version present

	if err := os.WriteFile(part, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := finalize(part, local, mtime); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(local) //nolint:gosec // G304: test temp dir
	if err != nil || string(data) != "new" {
		t.Fatalf("final content = %q, %v; want \"new\"", data, err)
	}
	st, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(mtime) {
		t.Fatalf("mtime = %v, want %v", st.ModTime(), mtime)
	}
	if exists(dir, "a.dll.part") || exists(dir, "a.dll.part.meta") {
		t.Fatal(".part/.meta left behind")
	}
}
