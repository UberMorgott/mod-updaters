package engine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/pkg/sftp"
	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

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

func TestFinalizeFailureKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "a.dll")
	mkTree(t, dir, "a.dll", "a.dll.part.meta") // .part missing: chtimes fails first
	if err := finalize(local+".part", local, time.Unix(1_700_000_000, 0)); err == nil {
		t.Fatal("finalize without .part succeeded")
	}
	if !exists(dir, "a.dll") || !exists(dir, "a.dll.part.meta") {
		t.Fatal("failed finalize touched the old file or the meta")
	}
}

func TestRetryDelay(t *testing.T) {
	s := time.Second
	want := []time.Duration{2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s, 30 * s}
	for i, w := range want {
		if got := retryDelay(i + 1); got != w {
			t.Errorf("retryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestRetryBudget(t *testing.T) {
	b := retryBudget{lastSize: 10}
	for i := range maxDownloadRetries {
		if _, ok := b.next(10); !ok {
			t.Fatalf("budget exhausted after %d failures, want %d", i, maxDownloadRetries)
		}
	}
	if _, ok := b.next(10); ok {
		t.Fatal("budget not exhausted without progress")
	}
	// Progress refills the budget and restarts the backoff.
	if d, ok := b.next(11); !ok || d != retryBaseDelay {
		t.Fatalf("after progress: next = %v, %v; want %v, true", d, ok, retryBaseDelay)
	}
}

func TestErrorClasses(t *testing.T) {
	noFile := &sftp.StatusError{Code: uint32(sftp.ErrSSHFxNoSuchFile)}
	failure := &sftp.StatusError{Code: 4} // SSH_FX_FAILURE
	cases := []struct {
		name            string
		err             error
		permanent, conn bool
	}{
		{"not exist", fmt.Errorf("open: %w", fs.ErrNotExist), true, false},
		{"permission", &fs.PathError{Op: "open", Err: fs.ErrPermission}, true, false},
		{"sftp no such file", noFile, true, false},
		{"sftp failure", failure, false, false},
		{"eof", io.EOF, false, true},
		{"timeout", os.ErrDeadlineExceeded, false, true},
	}
	for _, c := range cases {
		if got := isPermanent(c.err); got != c.permanent {
			t.Errorf("%s: isPermanent = %v, want %v", c.name, got, c.permanent)
		}
		if got := isConnError(c.err); got != c.conn {
			t.Errorf("%s: isConnError = %v, want %v", c.name, got, c.conn)
		}
	}
}

func TestReconcilePart(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "a.dll.part")
	info := fakeInfo{size: 100, mtime: time.Unix(1_700_000_000, 0)}
	if err := writeMeta(part, info); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, make([]byte, 40), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reconcilePart(part, info); err != nil || partOffset(part, 100) != 40 {
		t.Fatalf("unchanged remote: err=%v offset=%d, want resume at 40", err, partOffset(part, 100))
	}
	changed := fakeInfo{size: 200, mtime: info.mtime.Add(time.Hour)}
	if err := reconcilePart(part, changed); err != nil {
		t.Fatal(err)
	}
	if exists(dir, "a.dll.part") || !metaMatches(part, changed) {
		t.Fatal("changed remote: .part kept or meta not rewritten")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCopyProgressWriteError(t *testing.T) {
	ch := make(chan uint64, 10)
	if err := copyProgress(failWriter{}, strings.NewReader("data"), ch); err == nil {
		t.Fatal("write error swallowed")
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// update feeds msg to m and returns the resulting model.
func update(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	got, cmd := m.Update(msg)
	next, ok := got.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", got)
	}
	return next, cmd
}

func TestMenuKeys(t *testing.T) {
	fm, _ := update(t, newModel(Config{}), errorMsg{io.EOF})
	if !fm.menu || fm.menuReason == "" {
		t.Fatal("errorMsg did not open the menu")
	}

	m, cmd := update(t, fm, key("x"))
	if !m.menu || m.quitting || cmd != nil {
		t.Fatal("unknown key changed the menu")
	}

	m, cmd = update(t, fm, key("esc"))
	if !m.quitting || cmd == nil {
		t.Fatal("Esc did not quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Esc did not return tea.Quit")
	}

	for _, k := range []string{"r", "к"} {
		m, cmd = update(t, fm, key(k))
		if m.menu || m.quitting || cmd == nil {
			t.Fatalf("%q did not restart the sync", k)
		}
	}

	m, cmd = update(t, fm, key("enter")) // cmd launches the game: not executed here
	if !m.quitting || cmd == nil {
		t.Fatal("Enter did not launch")
	}
}

func TestPartialListingShowsMenu(t *testing.T) {
	if m, _ := update(t, newModel(Config{}), filesListedMsg{walkErr: fs.ErrPermission}); !m.menu || !m.syncDone {
		t.Fatal("partial listing launched silently")
	}
}
