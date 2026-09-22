package engine

import (
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// syncAndLaunch creates remote-present directories, runs the configured cleanup,
// then launches the game. The empty-listing safety guard short-circuits all
// cleanup (just launch) if the walk yielded zero entries.
func (m *model) syncAndLaunch() tea.Cmd {
	cfg := m.cfg
	allEntries := m.allEntries
	return func() tea.Msg {
		defer m.closeConnections()

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

		localRootAbs, _ := filepath.Abs(".")
		sftpDirs := buildSFTPDirSet(allEntries)

		for _, spec := range cfg.Cleanup {
			switch spec.Mode {
			case MirrorFiles:
				mirrorFiles(localRootAbs, spec.Path, allEntries)
			case MirrorSubdirs:
				mirrorSubdirs(localRootAbs, spec.Path, sftpDirs)
			}
		}

		launchGame(cfg)
		return nil
	}
}

// buildSFTPDirSet returns the set of forward-slash relative paths that are
// directories on SFTP this run.
func buildSFTPDirSet(entries []sftpEntry) map[string]struct{} {
	set := make(map[string]struct{})
	for _, e := range entries {
		if e.Info.IsDir() {
			set[e.RelSlash] = struct{}{}
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
	prefix := cleanupRel + "/"
	for _, e := range allEntries {
		if e.RelSlash == cleanupRel {
			continue // the root itself
		}
		if rel, ok := strings.CutPrefix(e.RelSlash, prefix); ok && rel != "" {
			keep[rel] = struct{}{}
		}
	}
	cleanDir(base, "", keep)
}

// cleanDir recursively deletes any entry under base/rel whose forward-slash
// relative path is not in keep. Surviving dirs are recursed into.
func cleanDir(base, rel string, keep map[string]struct{}) {
	entries, err := os.ReadDir(filepath.Join(base, rel))
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.ToSlash(filepath.Join(rel, e.Name()))
		if _, ok := keep[path]; !ok {
			_ = os.RemoveAll(filepath.Join(base, rel, e.Name())) // best-effort cleanup
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
	st, err := os.Stat(mirrorLocal)
	if err != nil || !st.IsDir() {
		return
	}
	entries, err := os.ReadDir(mirrorLocal)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue // never delete files at this level
		}
		relSlash := cleanupRel + "/" + e.Name()
		if _, ok := sftpDirs[relSlash]; ok {
			continue // matching dir on SFTP — keep
		}
		_ = os.RemoveAll(filepath.Join(mirrorLocal, e.Name())) // best-effort cleanup
	}
}
