package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	pathFlag := flag.String("path", "", "Path to a .mkv file or a directory containing .mkv files (required)")
	trackerFlag := flag.String("tracker", "", "Target tracker: lacale or torr9 (required)")
	configFlag := flag.String("config", "", "Path to config.toml (defaults to ~/.config/aatm-cli/config.toml)")
	flag.Parse()

	if *pathFlag == "" || *trackerFlag == "" {
		flag.Usage()
		os.Exit(1)
	}

	tracker := strings.ToLower(strings.TrimSpace(*trackerFlag))
	if tracker != "lacale" && tracker != "torr9" {
		fatalf("unknown tracker %q: must be lacale or torr9", tracker)
	}

	cfg, err := loadConfig(*configFlag)
	if err != nil {
		fatalf("config: %v", err)
	}

	// Validate required config fields
	if len(cfg.Torrent.Trackers) == 0 {
		fatalf("config: torrent.trackers must not be empty")
	}
	if cfg.QBittorrent.Host == "" {
		fatalf("config: qbittorrent.host is required")
	}
	if tracker == "lacale" {
		if cfg.LaCale.APIKey == "" {
			fatalf("config: lacale.api_key is required for tracker=lacale")
		}
		if cfg.LaCale.BaseURL == "" {
			fatalf("config: lacale.base_url is required for tracker=lacale")
		}
		if cfg.TMDB.APIKey == "" {
			fatalf("config: tmdb.api_key is required for tracker=lacale")
		}
		if err := ensureCategoryID(cfg); err != nil {
			fatalf("could not determine la-cale category: %v", err)
		}
	}

	// Collect .mkv files from the given path
	mkvFiles, err := collectMKVFiles(*pathFlag)
	if err != nil {
		fatalf("collecting files: %v", err)
	}
	if len(mkvFiles) == 0 {
		fatalf("no .mkv files found in %s", *pathFlag)
	}

	// Connect to qBittorrent once (shared for all files)
	qbt, err := newQBittorrentClient(cfg.QBittorrent)
	if err != nil {
		fatalf("qbittorrent client: %v", err)
	}
	logf("Connecting to qBittorrent at %s ...", cfg.QBittorrent.Host)
	if err := qbt.Login(); err != nil {
		fatalf("qBittorrent login: %v", err)
	}
	logf("qBittorrent: authenticated")

	for _, mkvPath := range mkvFiles {
		if err := processFile(mkvPath, tracker, cfg, qbt); err != nil {
			// Log error and continue with next file
			fmt.Fprintf(os.Stderr, "[ERROR] %s: %v\n", filepath.Base(mkvPath), err)
		}
	}
}

// processFile runs the full pipeline for a single .mkv file.
func processFile(mkvPath, tracker string, cfg *Config, qbt *qbittorrentClient) error {
	base := filepath.Base(mkvPath)
	logf("─── Processing: %s", base)

	// ── 1. Parse filename ────────────────────────────────────────────────────
	logf("  Parsing filename...")
	fi, err := ParseFilename(mkvPath)
	if err != nil {
		return fmt.Errorf("filename parsing: %w", err)
	}
	logf("  Series: %q  S%02dE%02d  %dp  %s", fi.SeriesTitle, fi.Season, fi.Episode, fi.Height, fi.Language)

	// ── 2. Generate NFO ──────────────────────────────────────────────────────
	logf("  Generating NFO with mediainfo...")
	nfoPath, nfoInfo, err := GenerateNFO(mkvPath)
	if err != nil {
		return fmt.Errorf("NFO generation: %w", err)
	}
	logf("  NFO written: %s", filepath.Base(nfoPath))

	// ── 3. Create .torrent ───────────────────────────────────────────────────
	logf("  Creating .torrent file...")
	torrentOpts := TorrentOptions{
		Trackers: cfg.Torrent.Trackers,
	}
	if tracker == "lacale" {
		torrentOpts.Source = "lacale"
	}
	torrentPath, infoHash, err := CreateTorrent(mkvPath, torrentOpts)
	if err != nil {
		return fmt.Errorf("torrent creation: %w", err)
	}
	logf("  Torrent written: %s (infohash: %s)", filepath.Base(torrentPath), infoHash)

	// ── 4. La-cale upload (tracker == lacale only) ───────────────────────────
	if tracker == "lacale" {
		if err := uploadToLaCale(mkvPath, fi, nfoInfo, nfoPath, torrentPath, cfg); err != nil {
			return fmt.Errorf("la-cale upload: %w", err)
		}

		// Delete NFO after successful la-cale upload
		logf("  Deleting NFO...")
		if err := os.Remove(nfoPath); err != nil {
			logf("  warning: could not delete NFO: %v", err)
		}
	}

	// ── 5. Add to qBittorrent ────────────────────────────────────────────────
	logf("  Adding torrent to qBittorrent (save path: %s)...", cfg.QBittorrent.SavePath)
	if err := qbt.AddAndConfigureTorrent(torrentPath, infoHash, cfg.QBittorrent.SavePath, cfg.QBittorrent.RatioLimit); err != nil {
		return fmt.Errorf("qBittorrent: %w", err)
	}
	logf("  Torrent added (ratio limit: %.1f)", cfg.QBittorrent.RatioLimit)

	// ── 6. Move video to qBittorrent save path ───────────────────────────────
	if cfg.QBittorrent.SavePath != "" {
		dest := filepath.Join(cfg.QBittorrent.SavePath, filepath.Base(mkvPath))
		logf("  Moving video to %s...", dest)
		if err := moveFile(mkvPath, dest); err != nil {
			return fmt.Errorf("moving video: %w", err)
		}
		logf("  Done.")
	}

	return nil
}

// uploadToLaCale handles steps 4a–4d: TMDB lookup, duplicate check, tag matching, upload.
func uploadToLaCale(mkvPath string, fi *FileInfo, nfoInfo *NFOInfo, nfoPath, torrentPath string, cfg *Config) error {
	lacale := newLaCaleClient(cfg.LaCale)
	tmdb := newTMDBClient(cfg.TMDB.APIKey)

	// 4a. Search TMDB
	logf("  Searching TMDB for %q...", fi.SeriesTitle)
	show, err := tmdb.SearchTVShow(fi.SeriesTitle)
	if err != nil {
		return fmt.Errorf("TMDB: %w", err)
	}
	tmdbID := fmt.Sprintf("%d", show.ID)
	coverURL := CoverURL(show.PosterPath)
	logf("  TMDB match: %q (ID %s)", show.Name, tmdbID)

	// 4b. Check for duplicate on la-cale
	if !cfg.LaCale.SkipDuplicateCheck {
		logf("  Checking for duplicates on la-cale...")
		existing, err := lacale.SearchExisting(fi.BaseName)
		if err != nil {
			logf("  warning: duplicate check failed: %v", err)
		} else if len(existing) > 0 {
			return fmt.Errorf("release already exists on la-cale: %s", existing[0].Link)
		}
	}

	// 4c. Fetch meta and match tags
	logf("  Fetching la-cale metadata (categories + tags)...")
	meta, err := lacale.GetMeta()
	if err != nil {
		return fmt.Errorf("la-cale meta: %w", err)
	}
	tags := MatchTags(meta, fi, nfoInfo)
	if len(tags) > 0 {
		logf("  Matched tags: %v", tags)
	} else {
		logf("  No tags matched (auto-tagging will run server-side)")
	}

	// 4d. Upload
	logf("  Uploading to la-cale...")
	uploadResp, err := lacale.Upload(UploadRequest{
		Title:       fi.BaseName,
		CategoryID:  cfg.LaCale.CategoryID,
		TmdbID:      tmdbID,
		TmdbType:    "show",
		CoverURL:    coverURL,
		Tags:        tags,
		TorrentPath: torrentPath,
		NFOPath:     nfoPath,
	})
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	logf("  Upload successful: %s", uploadResp.Link)
	return nil
}

// collectMKVFiles returns all .mkv files at the given path.
// If path is a file, returns [path]. If it's a directory, walks it.
func collectMKVFiles(path string) ([]string, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if !stat.IsDir() {
		if !strings.EqualFold(filepath.Ext(path), ".mkv") {
			return nil, fmt.Errorf("%s is not a .mkv file", path)
		}
		return []string{path}, nil
	}

	var files []string
	err = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.EqualFold(filepath.Ext(p), ".mkv") {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

// moveFile moves src to dst, handling cross-device scenarios via copy+delete.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Fallback: copy then remove
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func logf(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
