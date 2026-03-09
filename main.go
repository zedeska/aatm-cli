package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	pathFlag := flag.String("path", "", "Path to a .mkv file or a directory containing .mkv files (required)")
	trackerFlag := flag.String("tracker", "", "Target tracker: lacale or torr9 (required unless --watcher is set)")
	configFlag := flag.String("config", "", "Path to config.toml (defaults to ~/.config/aatm-cli/config.toml)")
	watcherFlag := flag.Bool("watcher", false, "Watch the directory given by --path and auto-upload new .mkv files to all configured trackers")
	flag.Parse()

	if *pathFlag == "" {
		flag.Usage()
		os.Exit(1)
	}

	cfg, err := loadConfig(*configFlag)
	if err != nil {
		fatalf("config: %v", err)
	}

	if cfg.QBittorrent.Host == "" {
		fatalf("config: qbittorrent.host is required")
	}

	// ── Watcher mode ──────────────────────────────────────────────────────────
	if *watcherFlag {
		stat, err := os.Stat(*pathFlag)
		if err != nil {
			fatalf("path: %v", err)
		}
		if !stat.IsDir() {
			fatalf("--watcher requires --path to be a directory")
		}

		// Pre-flight: ensure at least one tracker is usable
		hasLaCale := lacaleAvailable(cfg)
		hasTorr9 := torr9Available(cfg)
		if !hasLaCale && !hasTorr9 {
			fatalf("watcher mode requires at least one tracker to be fully configured (lacale or torr9)")
		}
		if hasLaCale {
			if err := ensureCategoryID(cfg); err != nil {
				fatalf("could not determine la-cale category: %v", err)
			}
			logf("Watcher: la-cale enabled")
		}
		if hasTorr9 {
			logf("Watcher: torr9 enabled")
		}

		qbt, err := newQBittorrentClient(cfg.QBittorrent)
		if err != nil {
			fatalf("qbittorrent client: %v", err)
		}
		logf("Connecting to qBittorrent at %s ...", cfg.QBittorrent.Host)
		if err := qbt.Login(); err != nil {
			fatalf("qBittorrent login: %v", err)
		}
		logf("qBittorrent: authenticated")

		watchDirectory(*pathFlag, cfg, qbt) // blocks forever
		return
	}

	// ── Single-run mode ───────────────────────────────────────────────────────
	if *trackerFlag == "" {
		flag.Usage()
		os.Exit(1)
	}

	tracker := strings.ToLower(strings.TrimSpace(*trackerFlag))
	if tracker != "lacale" && tracker != "torr9" {
		fatalf("unknown tracker %q: must be lacale or torr9", tracker)
	}

	// Validate required config fields
	if len(cfg.Torrent.Trackers) == 0 {
		fatalf("config: torrent.trackers must not be empty")
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
	if tracker == "torr9" {
		if cfg.Torr9.APIKey == "" {
			fatalf("config: torr9.api_key is required for tracker=torr9")
		}
		if cfg.TMDB.APIKey == "" {
			fatalf("config: tmdb.api_key is required for tracker=torr9")
		}
		if cfg.Torr9.Category == "" {
			fatalf("config: torr9.category is required for tracker=torr9 (e.g. \"Séries\")")
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
	// For torr9: no trackers and no source — torr9 returns its own torrent after upload.
	if tracker == "torr9" {
		torrentOpts = TorrentOptions{}
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

	// ── 4. Torr9 upload (tracker == torr9 only) ──────────────────────────────
	if tracker == "torr9" {
		newInfoHash, err := uploadToTorr9(mkvPath, fi, nfoInfo, torrentPath, cfg)
		if err != nil {
			return fmt.Errorf("torr9 upload: %w", err)
		}
		infoHash = newInfoHash

		// Delete NFO after successful torr9 upload
		logf("  Deleting NFO...")
		if err := os.Remove(nfoPath); err != nil {
			logf("  warning: could not delete NFO: %v", err)
		}

		// ── Move video BEFORE adding to qBittorrent ───────────────────────
		// qBittorrent must find the file at save_path at the moment the
		// torrent is added so that seeding starts immediately.
		if localMovePath(cfg.QBittorrent) != "" {
			dest := filepath.Join(localMovePath(cfg.QBittorrent), filepath.Base(mkvPath))
			logf("  Moving video to %s...", dest)
			if err := moveFile(mkvPath, dest); err != nil {
				return fmt.Errorf("moving video: %w", err)
			}
			mkvPath = dest // mark as moved so step 6 is skipped
		}
	}

	// ── 5. Add to qBittorrent ────────────────────────────────────────────────
	logf("  Adding torrent to qBittorrent (save path: %s)...", cfg.QBittorrent.SavePath)
	if err := qbt.AddAndConfigureTorrent(torrentPath, infoHash, cfg.QBittorrent.RatioLimit); err != nil {
		return fmt.Errorf("qBittorrent: %w", err)
	}
	logf("  Torrent added (ratio limit: %.1f)", cfg.QBittorrent.RatioLimit)

	// Delete the local .torrent file now that qBittorrent has it.
	if err := os.Remove(torrentPath); err != nil {
		logf("  warning: could not delete torrent file: %v", err)
	}

	// ── 6. Move video to save path (lacale / default) ─────────────────────────
	// Skipped for torr9: already moved above before the qBittorrent add.
	if tracker != "torr9" && localMovePath(cfg.QBittorrent) != "" {
		dest := filepath.Join(localMovePath(cfg.QBittorrent), filepath.Base(mkvPath))
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

// uploadToTorr9 handles the full torr9 upload pipeline:
// TMDB lookup → generate description → duplicate check → upload.
// It overwrites torrentPath with the torrent file returned by torr9 and
// returns torr9's info hash (which differs from the locally computed one).
func uploadToTorr9(mkvPath string, fi *FileInfo, nfoInfo *NFOInfo, torrentPath string, cfg *Config) (infoHash string, err error) {
	client := newTorr9Client(cfg.Torr9)
	tmdb := newTMDBClient(cfg.TMDB.APIKey)

	// Search TMDB
	logf("  Searching TMDB for %q...", fi.SeriesTitle)
	show, err := tmdb.SearchTVShow(fi.SeriesTitle)
	if err != nil {
		return "", fmt.Errorf("TMDB: %w", err)
	}
	logf("  TMDB match: %q (ID %d)", show.Name, show.ID)

	// File size for the request
	fileInfo, err := os.Stat(mkvPath)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}

	// Build tech_info
	containerFormat := nfoInfo.ContainerFormat
	if containerFormat == "" {
		containerFormat = "Matroska" // safe default for .mkv
	}
	techInfo := Torr9TechInfo{
		Source:       fi.Source,
		Resolution:   fmt.Sprintf("%dp", fi.Height),
		VideoFormat:  containerFormat,
		VideoCodec:   torr9CodecName(fi.Codec),
		VideoBitrate: nfoInfo.VideoBitrateRaw,
		Audio:        nfoInfo.BuildAudioString(),
		Subtitles:    nfoInfo.BuildSubtitlesString(),
	}

	// Generate description
	logf("  Generating description via torr9 API...")
	descResp, err := client.GenerateDescription(Torr9GenerateDescRequest{
		Title:       show.Name,
		NFO:         nfoInfo.Raw,
		Category:    "tv",
		TmdbID:      show.ID,
		ReleaseName: filepath.Base(mkvPath),
		SizeBytes:   fileInfo.Size(),
		FileCount:   1,
		TechInfo:    techInfo,
	})
	if err != nil {
		return "", fmt.Errorf("generate description: %w", err)
	}
	logf("  Description generated (title: %q)", descResp.Metadata.Title)

	// Duplicate check
	if !cfg.Torr9.SkipDuplicateCheck {
		logf("  Checking for duplicates on torr9...")
		dupResp, err := client.CheckDuplicate(fi.BaseName)
		if err != nil {
			logf("  warning: duplicate check failed: %v", err)
		} else if dupResp.IsDuplicate {
			return "", fmt.Errorf("release already exists on torr9 (%d match(es))", len(dupResp.Matches))
		}
	}

	// Upload
	logf("  Uploading to torr9...")
	uploadResp, err := client.Upload(Torr9UploadRequest{
		TorrentPath: torrentPath,
		Title:       fi.BaseName,
		Description: descResp.Description,
		NFO:         nfoInfo.Raw,
		Category:    cfg.Torr9.Category,
		Subcategory: cfg.Torr9.Subcategory,
		Tags:        buildTorr9Tags(fi),
		IsExclusive: false,
		IsAnonymous: false,
		TmdbID:      descResp.Metadata.TmdbID,
		ImdbID:      descResp.Metadata.ImdbID,
		TvdbID:      descResp.Metadata.TvdbID,
	})
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	logf("  Upload successful: torrent_id=%d (%s)", uploadResp.TorrentID, uploadResp.Message)

	// Decode the torrent file returned by torr9 (base64) and overwrite the
	// local torrent so qBittorrent gets the version with torr9's announce URL.
	if uploadResp.TorrentFile == "" {
		return "", fmt.Errorf("torr9 did not return a torrent file in the upload response")
	}
	torrentData, err := base64.StdEncoding.DecodeString(uploadResp.TorrentFile)
	if err != nil {
		return "", fmt.Errorf("decoding returned torrent (base64): %w", err)
	}
	if err := os.WriteFile(torrentPath, torrentData, 0o644); err != nil {
		return "", fmt.Errorf("saving returned torrent: %w", err)
	}
	logf("  Torrent file updated with torr9 version (infohash: %s)", uploadResp.InfoHash)

	return uploadResp.InfoHash, nil
}

// buildTorr9Tags builds the comma-separated tags string for the torr9 upload.
func buildTorr9Tags(fi *FileInfo) string {
	tags := []string{
		fmt.Sprintf("%dp", fi.Height),
		fi.Codec,
		fi.Source,
		fi.Language,
		fmt.Sprintf("S%02dE%02d", fi.Season, fi.Episode),
	}
	return strings.Join(tags, ", ")
}

// torr9CodecName maps codec names to the lowercase encoder-style names used by torr9.
func torr9CodecName(codec string) string {
	switch strings.ToUpper(codec) {
	case "H264", "X264", "AVC":
		return "x264"
	case "H265", "X265", "HEVC":
		return "x265"
	case "AV1":
		return "AV1"
	case "VP9":
		return "VP9"
	default:
		return strings.ToLower(codec)
	}
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

	out, err := os.Create(dst)
	if err != nil {
		in.Close()
		return err
	}

	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		in.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		in.Close()
		os.Remove(dst)
		return err
	}
	// Close the source before removing — on Windows an open handle blocks deletion.
	in.Close()
	return os.Remove(src)
}

// localMovePath returns the filesystem path to use when moving video files.
// If local_save_path is set it is used (for cases where the qBittorrent
// save_path is a remote Linux path and the local Windows path differs).
// Falls back to save_path when local_save_path is empty.
func localMovePath(cfg QBittorrentConfig) string {
	if cfg.LocalSavePath != "" {
		return cfg.LocalSavePath
	}
	return cfg.SavePath
}

func logf(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
