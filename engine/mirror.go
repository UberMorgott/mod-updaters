package engine

import (
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// syncAndLaunch creates remote-present directories, runs the configured cleanup,
// then launches the game. The empty-listing safety guard short-circuits all
// cleanup (just launch) if the walk yielded zero entries; an incomplete listing
// (walk error / rejected entry) skips cleanup too.
func (m *model) syncAndLaunch() tea.Cmd {
	cfg := m.cfg
	allEntries := m.allEntries
	complete := m.listingComplete
	c := m.conn
	return func() tea.Msg {
		c.Close()

		// Safety: if the remote listing is empty (remote dir missing or
		// unreachable), do NOT run any cleanup — that would wipe installed
		// mods. Just launch the game as-is.
		if len(allEntries) == 0 {
			launchGame(cfg)
			return nil
		}

		// Create directories that exist on SFTP (so empty dirs survive a sync).
		for _, e := range allEntries {
			if e.Info.IsDir() {
				_ = os.MkdirAll(e.LocalPath, 0755) // best-effort: a missing empty dir must not block launch
			}
		}

		if complete {
			localRootAbs, err := filepath.Abs(".")
			if err == nil {
				runCleanup(localRootAbs, cfg.Cleanup, allEntries)
			}
		}

		launchGame(cfg)
		return nil
	}
}

// runCleanup applies every Cleanup spec under localRoot against the SFTP listing.
func runCleanup(localRoot string, specs []CleanupSpec, allEntries []sftpEntry) {
	sftpDirs := buildSFTPDirSet(allEntries)
	for _, spec := range specs {
		switch spec.Mode {
		case MirrorFiles:
			mirrorFiles(localRoot, spec.Path, allEntries)
		case MirrorSubdirs:
			mirrorSubdirs(localRoot, spec.Path, sftpDirs)
		}
	}
}

// pathKey normalizes a forward-slash relative path for keep-set lookups. The
// game dirs live on case-insensitive NTFS: a local "foo.dll" IS the remote
// "Foo.dll" (the downloader already treated it as present), so it must never
// be deleted as an orphan.
func pathKey(relSlash string) string { return strings.ToLower(relSlash) }

// buildSFTPDirSet returns the set of (pathKey-normalized) forward-slash
// relative paths that are directories on SFTP this run.
func buildSFTPDirSet(entries []sftpEntry) map[string]struct{} {
	set := make(map[string]struct{})
	for _, e := range entries {
		if e.Info.IsDir() {
			set[pathKey(e.RelSlash)] = struct{}{}
		}
	}
	return set
}

// mirrorFiles applies a full mirror inside cleanupRel: any file or dir under it
// whose corresponding SFTP path doesn't exist is deleted (ported from UAR's
// cleanDir/remoteSet). Ports UAR semantics: builds a keep-set of rel paths under
// the cleanup root from allEntries, then recursively deletes orphans.
func mirrorFiles(localRoot, cleanupRel string, allEntries []sftpEntry) {
	base := filepath.Join(localRoot, filepath.FromSlash(cleanupRel))

	// Keep-set: every SFTP entry under the cleanup root, expressed relative to
	// that root in forward-slash form.
	keep := make(map[string]struct{})
	prefix := pathKey(cleanupRel) + "/"
	for _, e := range allEntries {
		if rel, ok := strings.CutPrefix(pathKey(e.RelSlash), prefix); ok && rel != "" {
			keep[rel] = struct{}{}
		}
	}
	cleanDir(base, "", keep)
}

// cleanDir recursively deletes any entry under base/rel whose forward-slash
// relative path is not in keep. Surviving dirs are recursed into.
func cleanDir(base, rel string, keep map[string]struct{}) {
	entries, err := os.ReadDir(filepath.Join(base, filepath.FromSlash(rel)))
	if err != nil {
		return
	}
	for _, e := range entries {
		path := e.Name()
		if rel != "" {
			path = rel + "/" + e.Name()
		}
		if _, ok := keep[pathKey(path)]; !ok {
			_ = os.RemoveAll(filepath.Join(base, filepath.FromSlash(path))) // best-effort cleanup
		} else if e.IsDir() {
			cleanDir(base, path, keep)
		}
	}
}

// mirrorSubdirs deletes any immediate subdirectory of localRoot/cleanupRel that
// is NOT present as a directory on SFTP at the same relative path. Files
// directly inside that dir are NEVER touched (ported from WUR's
// cleanupFullMirror/buildSFTPDirSet).
func mirrorSubdirs(localRoot, cleanupRel string, sftpDirs map[string]struct{}) {
	mirrorLocal := filepath.Join(localRoot, filepath.FromSlash(cleanupRel))
	entries, err := os.ReadDir(mirrorLocal)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue // never delete files at this level
		}
		if _, ok := sftpDirs[pathKey(cleanupRel+"/"+e.Name())]; ok {
			continue // matching dir on SFTP — keep
		}
		_ = os.RemoveAll(filepath.Join(mirrorLocal, e.Name())) // best-effort cleanup
	}
}
