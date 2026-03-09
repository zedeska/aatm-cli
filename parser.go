package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// FileInfo holds the parsed metadata extracted from a release filename.
type FileInfo struct {
	// Original base name without extension (e.g. "My.Show.S01E03.VOSTFR.1080p.WEB-DL.H264-ZED")
	BaseName string
	// Full path to the .mkv file
	Path string
	// Series title with spaces (e.g. "My Show")
	SeriesTitle string
	// Season number (e.g. 1)
	Season int
	// Episode number (e.g. 3)
	Episode int
	// Vertical resolution (e.g. 1080)
	Height int
	// Language tag extracted from filename (e.g. "VOSTFR")
	Language string
	// Source tag (e.g. "WEB-DL")
	Source string
	// Codec tag (e.g. "H264")
	Codec string
	// Release group (e.g. "ZED")
	Group string
}

// Pattern: ${seriesTitle}.S${season}E${episode}.VOSTFR.${height}p.WEB-DL.H264-ZED
// e.g.  My.Show.Name.S02E05.VOSTFR.1080p.WEB-DL.H264-ZED
var filenameRe = regexp.MustCompile(
	`(?i)^(.+?)\.S(\d{2})E(\d{2,3})\.(VOSTFR|VFF|VF|VOSTA)\.([\d]+)p\.(WEB-DL|WEBRIP|BLURAY|HDTV|DVDRIP)\.(H264|H265|X264|X265|HEVC|AVC)-(.+)$`,
)

// ParseFilename parses a release filename (with or without .mkv extension)
// and returns a populated FileInfo.
func ParseFilename(path string) (*FileInfo, error) {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	matches := filenameRe.FindStringSubmatch(name)
	if matches == nil {
		return nil, fmt.Errorf("filename %q does not match expected release pattern", name)
	}

	season, _ := strconv.Atoi(matches[2])
	episode, _ := strconv.Atoi(matches[3])
	height, _ := strconv.Atoi(matches[5])

	seriesTitle := strings.ReplaceAll(matches[1], ".", " ")

	return &FileInfo{
		BaseName:    name,
		Path:        path,
		SeriesTitle: seriesTitle,
		Season:      season,
		Episode:     episode,
		Height:      height,
		Language:    strings.ToUpper(matches[4]),
		Source:      strings.ToUpper(matches[6]),
		Codec:       strings.ToUpper(matches[7]),
		Group:       matches[8],
	}, nil
}
