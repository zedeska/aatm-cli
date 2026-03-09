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

// lacaleAvailable returns true if the config contains the minimum fields needed
// for a la-cale upload.
func lacaleAvailable(cfg *Config) bool {
	return cfg.LaCale.APIKey != "" &&
		cfg.LaCale.BaseURL != "" &&
		cfg.TMDB.APIKey != "" &&
		len(cfg.Torrent.Trackers) > 0
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
					go processFileDualTracker(path, cfg, qbt)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

// processFileDualTracker runs the full upload pipeline for a single .mkv file
// and publishes it to every configured tracker (la-cale and/or torr9).
// A failure on one tracker is non-fatal: the other upload still proceeds.
func processFileDualTracker(mkvPath string, cfg *Config, qbt *qbittorrentClient) {
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

	// ── 3. Create torrent for la-cale (with source + trackers) ───────────────
	stem := mkvPath[:len(mkvPath)-len(filepath.Ext(mkvPath))]
	lacaleTorrentPath := stem + "-lacale.torrent"
	torr9TorrentPath := stem + "-torr9.torrent"

	var lacaleTorrentOK, torr9TorrentOK bool
	var lacaleInfoHash string

	if lacaleAvailable(cfg) {
		_, lacaleInfoHash, err = CreateTorrent(mkvPath, TorrentOptions{
			Trackers:   cfg.Torrent.Trackers,
			Source:     "lacale",
			OutputPath: lacaleTorrentPath,
		})
		if err != nil {
			logf("  [WARN] could not create la-cale torrent: %v", err)
		} else {
			lacaleTorrentOK = true
			logf("  La-cale torrent: %s", filepath.Base(lacaleTorrentPath))
		}
	}

	if torr9Available(cfg) {
		_, _, err = CreateTorrent(mkvPath, TorrentOptions{OutputPath: torr9TorrentPath})
		if err != nil {
			logf("  [WARN] could not create torr9 torrent: %v", err)
		} else {
			torr9TorrentOK = true
			logf("  Torr9 torrent: %s", filepath.Base(torr9TorrentPath))
		}
	}

	// ── 4. Upload to la-cale ──────────────────────────────────────────────────
	lacaleUploadOK := false
	if lacaleTorrentOK {
		if err := uploadToLaCale(mkvPath, fi, nfoInfo, nfoPath, lacaleTorrentPath, cfg); err != nil {
			logf("  [WARN] la-cale upload failed: %v", err)
		} else {
			lacaleUploadOK = true
			logf("  La-cale upload: OK")
		}
	}

	// ── 5. Upload to torr9 ────────────────────────────────────────────────────
	torr9UploadOK := false
	var torr9InfoHash string
	if torr9TorrentOK {
		torr9InfoHash, err = uploadToTorr9(mkvPath, fi, nfoInfo, torr9TorrentPath, cfg)
		if err != nil {
			logf("  [WARN] torr9 upload failed: %v", err)
		} else {
			torr9UploadOK = true
			logf("  Torr9 upload: OK (infohash: %s)", torr9InfoHash)
		}
	}

	// ── 6. Delete NFO ─────────────────────────────────────────────────────────
	if err := os.Remove(nfoPath); err != nil {
		logf("  warning: could not delete NFO: %v", err)
	}

	if !lacaleUploadOK && !torr9UploadOK {
		logf("[ERROR] %s: all uploads failed — skipping qBittorrent and file move", base)
		// Clean up torrent files before returning
		if lacaleTorrentOK {
			os.Remove(lacaleTorrentPath)
		}
		if torr9TorrentOK {
			os.Remove(torr9TorrentPath)
		}
		return
	}

	// ── 7. Move video to save path ────────────────────────────────────────────
	// torr9 requires the file to be present at the save path before the torrent
	// is added to qBittorrent.  Moving it here satisfies that for both trackers.
	movePath := localMovePath(cfg.QBittorrent)
	if movePath != "" {
		dest := filepath.Join(movePath, filepath.Base(mkvPath))
		logf("  Moving video to %s...", dest)
		if err := moveFile(mkvPath, dest); err != nil {
			logf("[ERROR] %s: moving video: %v", base, err)
			// Clean up torrent files before returning
			if lacaleTorrentOK {
				os.Remove(lacaleTorrentPath)
			}
			if torr9TorrentOK {
				os.Remove(torr9TorrentPath)
			}
			return
		}
	}

	// ── 8. Add torrents to qBittorrent ────────────────────────────────────────
	if lacaleUploadOK {
		logf("  Adding la-cale torrent to qBittorrent...")
		if err := qbt.AddAndConfigureTorrent(lacaleTorrentPath, lacaleInfoHash, cfg.QBittorrent.RatioLimit); err != nil {
			logf("  warning: qBittorrent (la-cale): %v", err)
		}
	}
	if torr9UploadOK {
		logf("  Adding torr9 torrent to qBittorrent...")
		if err := qbt.AddAndConfigureTorrent(torr9TorrentPath, torr9InfoHash, cfg.QBittorrent.RatioLimit); err != nil {
			logf("  warning: qBittorrent (torr9): %v", err)
		}
	}

	// ── 9. Delete local torrent files ─────────────────────────────────────────
	if lacaleTorrentOK {
		if err := os.Remove(lacaleTorrentPath); err != nil {
			logf("  warning: could not delete la-cale torrent: %v", err)
		}
	}
	if torr9TorrentOK {
		if err := os.Remove(torr9TorrentPath); err != nil {
			logf("  warning: could not delete torr9 torrent: %v", err)
		}
	}

	logf("  Done: %s", base)
	fmt.Println()
}
