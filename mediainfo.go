package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NFOInfo holds key technical data extracted from the mediainfo text output.
type NFOInfo struct {
	Raw          string
	VideoCodec   string
	AudioCodec   string
	AudioLangs   []string // all audio track languages (full names, e.g. "Japanese")
	SubtitleLangs []string // all subtitle track languages (full names, e.g. "French")
	HDRFormat    string   // "SDR", "HDR10", "HDR", "DV", …
	DurationMins int
	Width        int
	Height       int
}

// GenerateNFO runs `mediainfo` on the given video file, writes the output to
// a .nfo file next to the video, and returns the parsed NFO metadata.
func GenerateNFO(videoPath string) (nfoPath string, info *NFOInfo, err error) {
	if _, err := exec.LookPath("mediainfo"); err != nil {
		return "", nil, fmt.Errorf("mediainfo is not installed or not in PATH: %w", err)
	}

	out, err := exec.Command("mediainfo", videoPath).Output()
	if err != nil {
		return "", nil, fmt.Errorf("mediainfo failed: %w", err)
	}

	raw := string(out)

	// Replace the full path on the "Complete name" line with just the filename
	// to avoid leaking directory structure in the NFO.
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if key, _, found := strings.Cut(line, ":"); found {
			if strings.TrimSpace(key) == "Complete name" {
				lines[i] = key + ": " + filepath.Base(videoPath)
				break
			}
		}
	}
	raw = strings.Join(lines, "\n")

	base := filepath.Base(videoPath)
	ext := filepath.Ext(base)
	nfoName := strings.TrimSuffix(base, ext) + ".nfo"
	nfoPath = filepath.Join(filepath.Dir(videoPath), nfoName)

	if err := os.WriteFile(nfoPath, []byte(raw), 0o644); err != nil {
		return "", nil, fmt.Errorf("failed to write NFO file: %w", err)
	}

	parsed := parseNFO(raw)
	return nfoPath, parsed, nil
}

// mediainfo section types
type nfoSection int

const (
	secUnknown nfoSection = iota
	secGeneral
	secVideo
	secAudio
	secText
)

// parseNFO extracts structured fields from the plain-text mediainfo output
// using a section-aware approach so that audio and subtitle tracks are
// distinguished correctly.
func parseNFO(raw string) *NFOInfo {
	info := &NFOInfo{Raw: raw}

	sec := secUnknown
	seenLangs := map[string]bool{}

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// ── Section header detection ──────────────────────────────────────
		// mediainfo section headers contain no ":" and match known names.
		if !strings.Contains(trimmed, ":") {
			base := strings.TrimRight(trimmed, " 0123456789#") // strip " #1" suffix
			base = strings.TrimSpace(base)
			switch strings.ToLower(base) {
			case "general":
				sec = secGeneral
			case "video":
				sec = secVideo
			case "audio":
				sec = secAudio
			case "text":
				sec = secText
			default:
				sec = secUnknown
			}
			continue
		}

		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		lkey := strings.ToLower(key)

		switch sec {
		case secGeneral:
			if lkey == "duration" && info.DurationMins == 0 {
				info.DurationMins = parseDurationMins(value)
			}

		case secVideo:
			switch lkey {
			case "format":
				if info.VideoCodec == "" && isVideoCodec(value) {
					info.VideoCodec = normalizeVideoCodec(value)
				}
			case "width":
				if info.Width == 0 {
					var w int
					fmt.Sscanf(strings.ReplaceAll(value, " ", ""), "%d", &w)
					info.Width = w
				}
			case "height":
				if info.Height == 0 {
					var h int
					fmt.Sscanf(strings.ReplaceAll(value, " ", ""), "%d", &h)
					info.Height = h
				}
			case "hdr format":
				if info.HDRFormat == "" {
					info.HDRFormat = normalizeHDRFormat(value)
				}
			case "transfer characteristics":
				if info.HDRFormat == "" {
					info.HDRFormat = hdrFromTransfer(value)
				}
			}

		case secAudio:
			switch lkey {
			case "format":
				if info.AudioCodec == "" && isAudioCodec(value) {
					info.AudioCodec = normalizeAudioCodec(value)
				}
			case "language":
				lang := normalizeLangName(value)
				if lang != "" && !seenLangs["a:"+lang] {
					info.AudioLangs = append(info.AudioLangs, lang)
					seenLangs["a:"+lang] = true
				}
			}

		case secText:
			if lkey == "language" {
				lang := normalizeLangName(value)
				if lang != "" && !seenLangs["s:"+lang] {
					info.SubtitleLangs = append(info.SubtitleLangs, lang)
					seenLangs["s:"+lang] = true
				}
			}
		}
	}

	// Default to SDR when no HDR info was found
	if info.HDRFormat == "" {
		info.HDRFormat = "SDR"
	}

	return info
}

// normalizeHDRFormat maps the mediainfo "HDR format" field to a short label.
func normalizeHDRFormat(v string) string {
	lower := strings.ToLower(v)
	switch {
	case strings.Contains(lower, "dolby vision"):
		return "DV"
	case strings.Contains(lower, "hdr10+"):
		return "HDR10+"
	case strings.Contains(lower, "hdr10"):
		return "HDR10"
	case strings.Contains(lower, "hlg"):
		return "HLG"
	case strings.Contains(lower, "sdr"):
		return "SDR"
	}
	return strings.ToUpper(strings.SplitN(v, ",", 2)[0]) // take first part
}

// hdrFromTransfer infers HDR/SDR from the "Transfer characteristics" field.
func hdrFromTransfer(v string) string {
	lower := strings.ToLower(v)
	switch {
	case strings.Contains(lower, "pq"):
		return "HDR"
	case strings.Contains(lower, "hlg"):
		return "HLG"
	case strings.Contains(lower, "bt.709"), strings.Contains(lower, "bt709"):
		return "SDR"
	}
	return ""
}

// normalizeLangName converts a mediainfo language string to a canonical title-case
// full name (e.g. "Japanese", "French", "English").
func normalizeLangName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	// Map ISO 639-1/2 codes to full names
	if full, ok := isoToLangName[strings.ToLower(v)]; ok {
		return full
	}
	// Already a full name — title-case it
	return strings.Title(strings.ToLower(v)) //nolint:staticcheck
}

// LangToISOCode returns the 2-letter uppercase ISO code for a full language name.
// Used to build "ST:XX" subtitle tag candidates.
func langToISOCode(lang string) string {
	if code, ok := langNameToISO[strings.ToLower(lang)]; ok {
		return code
	}
	// If it looks like a 2-letter code already, uppercase it
	if len(lang) == 2 {
		return strings.ToUpper(lang)
	}
	return ""
}

// ── language tables ──────────────────────────────────────────────────────────

var isoToLangName = map[string]string{
	"fr": "French", "fre": "French",
	"en": "English", "eng": "English",
	"ja": "Japanese", "jpn": "Japanese",
	"de": "German", "deu": "German", "ger": "German",
	"es": "Spanish", "spa": "Spanish",
	"it": "Italian", "ita": "Italian",
	"pt": "Portuguese", "por": "Portuguese",
	"ru": "Russian", "rus": "Russian",
	"zh": "Chinese", "zho": "Chinese", "chi": "Chinese",
	"ko": "Korean", "kor": "Korean",
	"ar": "Arabic", "ara": "Arabic",
	"nl": "Dutch", "nld": "Dutch",
	"pl": "Polish", "pol": "Polish",
	"sv": "Swedish", "swe": "Swedish",
	"tr": "Turkish", "tur": "Turkish",
}

var langNameToISO = map[string]string{
	"french": "FR", "english": "EN", "japanese": "JA",
	"german": "DE", "spanish": "ES", "italian": "IT",
	"portuguese": "PT", "russian": "RU", "chinese": "ZH",
	"korean": "KO", "arabic": "AR", "dutch": "NL",
	"polish": "PL", "swedish": "SV", "turkish": "TR",
}

// ── codec helpers ─────────────────────────────────────────────────────────────

func isVideoCodec(v string) bool {
	v = strings.ToLower(v)
	return strings.Contains(v, "avc") ||
		strings.Contains(v, "hevc") ||
		strings.Contains(v, "h.264") ||
		strings.Contains(v, "h.265") ||
		strings.Contains(v, "vp9") ||
		strings.Contains(v, "av1")
}

func isAudioCodec(v string) bool {
	v = strings.ToLower(v)
	return strings.Contains(v, "aac") ||
		strings.Contains(v, "ac-3") ||
		strings.Contains(v, "dts") ||
		strings.Contains(v, "flac") ||
		strings.Contains(v, "opus") ||
		strings.Contains(v, "truehd")
}

func normalizeVideoCodec(v string) string {
	v = strings.ToLower(v)
	switch {
	case strings.Contains(v, "avc"), strings.Contains(v, "h.264"):
		return "H264"
	case strings.Contains(v, "hevc"), strings.Contains(v, "h.265"):
		return "H265"
	case strings.Contains(v, "vp9"):
		return "VP9"
	case strings.Contains(v, "av1"):
		return "AV1"
	}
	return strings.ToUpper(v)
}

func normalizeAudioCodec(v string) string {
	v = strings.ToLower(v)
	switch {
	case strings.Contains(v, "aac"):
		return "AAC"
	case strings.Contains(v, "ac-3"), strings.Contains(v, "dolby digital"):
		return "AC3"
	case strings.Contains(v, "truehd"):
		return "TrueHD"
	case strings.Contains(v, "dts"):
		return "DTS"
	case strings.Contains(v, "flac"):
		return "FLAC"
	case strings.Contains(v, "opus"):
		return "Opus"
	}
	return strings.ToUpper(v)
}

// parseDurationMins converts a mediainfo duration string to minutes.
func parseDurationMins(s string) int {
	s = strings.ToLower(s)
	if strings.Contains(s, "min") {
		var m int
		fmt.Sscanf(s, "%d", &m)
		return m
	}
	var ms int
	clean := strings.ReplaceAll(s, " ", "")
	fmt.Sscanf(clean, "%d", &ms)
	if ms > 0 {
		return ms / 60000
	}
	return 0
}
