package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// seenFiles tracks .mkv paths that have already been queued during this
// session, preventing the same file from being processed more than once.
var (
	seenMu    sync.Mutex
	seenFiles = make(map[string]struct{})
)

// markSeen adds path to the seen set. Returns true if newly added (not yet seen).
func markSeen(path string) bool {
	seenMu.Lock()
	defer seenMu.Unlock()
	if _, exists := seenFiles[path]; exists {
		return false
	}
	seenFiles[path] = struct{}{}
	return true
}

// torr9Available returns true if the config contains the minimum fields needed
// for a torr9 upload.
func torr9Available(cfg *Config) bool {
	return cfg.Torr9.APIKey != "" &&
		cfg.Torr9.Category != "" &&
		cfg.TMDB.APIKey != ""
}

// watchDirectory polls dir every 5 seconds and launches a goroutine for each
// new .mkv file discovered.  It runs indefinitely.
func watchDirectory(dir string, cfg *Config, qbt *qbittorrentClient) {
	logf("Watcher: monitoring %s (polling every 5 s)", dir)
	for {
		files, err := collectMKVFiles(dir)
		if err != nil {
			logf("Watcher: error scanning directory: %v", err)
		} else {
			for _, path := range files {
				if markSeen(path) {
					go processFileWatcher(path, cfg, qbt)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

// processFileWatcher runs the full torr9 upload pipeline for a single .mkv file.
func processFileWatcher(mkvPath string, cfg *Config, qbt *qbittorrentClient) {
	base := filepath.Base(mkvPath)
	logf("─── [watcher] Processing: %s", base)

	// ── 1. Parse filename ─────────────────────────────────────────────────────
	logf("  Parsing filename...")
	fi, err := ParseFilename(mkvPath)
	if err != nil {
		logf("[ERROR] %s: filename parsing: %v", base, err)
		return
	}
	logf("  Series: %q  S%02dE%02d  %dp  %s", fi.SeriesTitle, fi.Season, fi.Episode, fi.Height, fi.Language)

	// ── 2. Generate NFO ───────────────────────────────────────────────────────
	logf("  Generating NFO with mediainfo...")
	nfoPath, nfoInfo, err := GenerateNFO(mkvPath)
	if err != nil {
		logf("[ERROR] %s: NFO generation: %v", base, err)
		return
	}
	logf("  NFO written: %s", filepath.Base(nfoPath))

	// Validate subtitles: must have French + English
	if err := validateSubtitles(nfoInfo); err != nil {
		logf("  [ERROR] %s: %v — cleaning up", base, err)
		os.Remove(nfoPath)
		return
	}

	// ── 3. Create torr9 torrent (no trackers) ─────────────────────────────────
	stem := mkvPath[:len(mkvPath)-len(filepath.Ext(mkvPath))]
	torr9TorrentPath := stem + "-torr9.torrent"

	var torr9TorrentOK bool
	var torr9InfoHash string

	if torr9Available(cfg) {
		_, torr9InfoHash, err = CreateTorrent(mkvPath, TorrentOptions{OutputPath: torr9TorrentPath})
		if err != nil {
			logf("  [WARN] could not create torr9 torrent: %v", err)
		} else {
			torr9TorrentOK = true
			logf("  Torr9 torrent: %s", filepath.Base(torr9TorrentPath))
		}
	}

	// ── 4. Upload to torr9 ────────────────────────────────────────────────────
	torr9UploadOK := false
	if torr9TorrentOK {
		torr9InfoHash, err = uploadToTorr9(mkvPath, fi, nfoInfo, torr9TorrentPath, cfg)
		if err != nil {
			logf("  [WARN] torr9 upload failed: %v", err)
		} else {
			torr9UploadOK = true
			logf("  Torr9 upload: OK (infohash: %s)", torr9InfoHash)
		}
	}

	// ── 5. Delete NFO ─────────────────────────────────────────────────────────
	if err := os.Remove(nfoPath); err != nil {
		logf("  warning: could not delete NFO: %v", err)
	}

	if !torr9UploadOK {
		logf("[ERROR] %s: torr9 upload failed — skipping qBittorrent and file move", base)
		if torr9TorrentOK {
			os.Remove(torr9TorrentPath)
		}
		return
	}

	// ── 6. Move video to save path ────────────────────────────────────────────
	// torr9 requires the file to be present at the save path before the torrent
	// is added to qBittorrent.
	movePath := localMovePath(cfg.QBittorrent)
	if movePath != "" {
		dest := filepath.Join(movePath, filepath.Base(mkvPath))
		logf("  Moving video to %s...", dest)
		if err := moveFile(mkvPath, dest); err != nil {
			logf("[ERROR] %s: moving video: %v", base, err)
			if torr9TorrentOK {
				os.Remove(torr9TorrentPath)
			}
			return
		}
	}

	// ── 7. Add torrent to qBittorrent ────────────────────────────────────────
	if torr9UploadOK {
		logf("  Adding torrent to qBittorrent...")
		if err := qbt.AddAndConfigureTorrent(torr9TorrentPath, torr9InfoHash, cfg.QBittorrent.RatioLimit); err != nil {
			logf("  warning: qBittorrent: %v", err)
		}
	}

	// ── 8. Delete local torrent file ─────────────────────────────────────────
	if torr9TorrentOK {
		if err := os.Remove(torr9TorrentPath); err != nil {
			logf("  warning: could not delete torrent: %v", err)
		}
	}

	logf("  Done: %s", base)
	fmt.Println()
}
