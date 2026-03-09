package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// lacaleClient wraps the La Cale external API.
type lacaleClient struct {
	cfg  LaCaleConfig
	http *http.Client
}

func newLaCaleClient(cfg LaCaleConfig) *lacaleClient {
	return &lacaleClient{
		cfg:  cfg,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// ── Meta ─────────────────────────────────────────────────────────────────────

// MetaResponse mirrors the /api/external/meta response schema.
type MetaResponse struct {
	Categories    []Category    `json:"categories"`
	TagGroups     []TagGroup    `json:"tagGroups"`
	UngroupedTags []Tag         `json:"ungroupedTags"`
}

type Category struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Slug     string     `json:"slug"`
	Icon     *string    `json:"icon"`
	ParentID *string    `json:"parentId"`
	Children []Category `json:"children"`
}

type TagGroup struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Order int    `json:"order"`
	Tags  []Tag  `json:"tags"`
}

type Tag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// GetMeta fetches all categories, tag groups and standalone tags.
func (c *lacaleClient) GetMeta() (*MetaResponse, error) {
	req, err := http.NewRequest(http.MethodGet, c.cfg.BaseURL+"/api/external/meta", nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("la-cale meta request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("la-cale meta returned %d", resp.StatusCode)
	}

	var meta MetaResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decoding la-cale meta: %w", err)
	}
	return &meta, nil
}

// ── Search / duplicate check ─────────────────────────────────────────────────

// ExternalResult mirrors the ExternalResult schema.
type ExternalResult struct {
	Title        string `json:"title"`
	GUID         string `json:"guid"`
	Size         int64  `json:"size"`
	PubDate      string `json:"pubDate"`
	Link         string `json:"link"`
	DownloadLink string `json:"downloadLink"`
	Category     string `json:"category"`
	Seeders      int    `json:"seeders"`
	Leechers     int    `json:"leechers"`
	InfoHash     string `json:"infoHash"`
}

// SearchExisting checks whether a release with the given query already exists.
// Returns the matching results (may be empty).
func (c *lacaleClient) SearchExisting(query string) ([]ExternalResult, error) {
	endpoint := c.cfg.BaseURL + "/api/external"

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("q", query)
	req.URL.RawQuery = q.Encode()
	c.setAuth(req) // must be called after query params are set

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("la-cale search failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("la-cale search returned %d", resp.StatusCode)
	}

	var results []ExternalResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("decoding la-cale search response: %w", err)
	}
	return results, nil
}

// ── Upload ───────────────────────────────────────────────────────────────────

// UploadRequest holds all the fields needed for /api/external/upload.
type UploadRequest struct {
	Title      string
	CategoryID string
	Description string
	TmdbID     string
	TmdbType   string
	CoverURL    string
	Tags       []string
	TorrentPath string
	NFOPath    string
}

// UploadResponse mirrors the UploadResponse schema.
type LaCaleUploadResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Link    string `json:"link"`
}

// Upload submits a new torrent to la-cale.
func (c *lacaleClient) Upload(req UploadRequest) (*LaCaleUploadResponse, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	writeField := func(key, value string) error {
		if value == "" {
			return nil
		}
		return mw.WriteField(key, value)
	}

	if err := writeField("title", req.Title); err != nil {
		return nil, err
	}
	if err := writeField("categoryId", req.CategoryID); err != nil {
		return nil, err
	}
	if err := writeField("description", req.Description); err != nil {
		return nil, err
	}
	if err := writeField("tmdbId", req.TmdbID); err != nil {
		return nil, err
	}
	if err := writeField("tmdbType", req.TmdbType); err != nil {
		return nil, err
	}
	if err := writeField("coverUrl", req.CoverURL); err != nil {
		return nil, err
	}
	for _, tag := range req.Tags {
		if err := mw.WriteField("tags", tag); err != nil {
			return nil, err
		}
	}

	// Attach .torrent file
	if err := attachFile(mw, "file", req.TorrentPath); err != nil {
		return nil, fmt.Errorf("attaching torrent: %w", err)
	}

	// Attach .nfo file
	if err := attachFile(mw, "nfoFile", req.NFOPath); err != nil {
		return nil, fmt.Errorf("attaching NFO: %w", err)
	}

	if err := mw.Close(); err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.cfg.BaseURL+"/api/external/upload", &body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	c.setAuth(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("la-cale upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("duplicate torrent (409): %s", string(respBody))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("la-cale upload returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result LaCaleUploadResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding upload response: %w", err)
	}
	return &result, nil
}

// ── Tag matching ─────────────────────────────────────────────────────────────

// MatchTags returns the tag slugs from the meta response that match the
// known attributes of this release (resolution, source, language, codec,
// audio languages, subtitle languages, HDR format).
func MatchTags(meta *MetaResponse, fi *FileInfo, nfo *NFOInfo) []string {
	// Build lookup: normalised key → slug, indexed by BOTH name and slug
	// so we match regardless of how la-cale stores the tag.
	all := map[string]string{}
	for _, tg := range meta.TagGroups {
		for _, t := range tg.Tags {
			all[normalizeTagKey(t.Name)] = t.Slug
			all[normalizeTagKey(t.Slug)] = t.Slug
		}
	}
	for _, t := range meta.UngroupedTags {
		all[normalizeTagKey(t.Name)] = t.Slug
		all[normalizeTagKey(t.Slug)] = t.Slug
	}

	// Build candidate strings from filename metadata
	candidates := []string{
		fmt.Sprintf("%dp", fi.Height),
		fi.Language,
		fi.Source,
		fi.Codec,
	}

	// Add NFO-derived candidates
	if nfo != nil {
		if nfo.AudioCodec != "" {
			candidates = append(candidates, nfo.AudioCodec)
		}
		if nfo.VideoCodec != "" && nfo.VideoCodec != fi.Codec {
			candidates = append(candidates, nfo.VideoCodec)
		}
		if nfo.HDRFormat != "" {
			candidates = append(candidates, nfo.HDRFormat)
		}
		// Audio languages (e.g. "Japanese")
		for _, lang := range nfo.AudioLangs {
			candidates = append(candidates, lang)
		}
		// Subtitle languages as both full name and "ST:XX" format
		for _, lang := range nfo.SubtitleLangs {
			candidates = append(candidates, lang)
			if code := langToISOCode(lang); code != "" {
				candidates = append(candidates, "ST:"+code)
			}
		}
	}

	seen := map[string]bool{}
	var matched []string
	for _, c := range candidates {
		slug, ok := all[normalizeTagKey(c)]
		if ok && !seen[slug] {
			matched = append(matched, slug)
			seen[slug] = true
		}
	}
	return matched
}

// normalizeTagKey strips all non-alphanumeric characters and lowercases so that
// "WEB-DL", "Web DL", "WEBDL" and "web-dl" all map to the same key "webdl".
func normalizeTagKey(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (c *lacaleClient) setAuth(r *http.Request) {
	r.Header.Set("X-Api-Key", c.cfg.APIKey)

	// Also pass the key as a query param so it survives any server-side redirect.
	q := r.URL.Query()
	q.Set("apikey", c.cfg.APIKey)
	r.URL.RawQuery = q.Encode()
}

func attachFile(mw *multipart.Writer, fieldName, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	part, err := mw.CreateFormFile(fieldName, filepath.Base(filePath))
	if err != nil {
		return err
	}

	_, err = io.Copy(part, f)
	return err
}

// ── First-run: category auto-detection ───────────────────────────────────────

// ensureCategoryID checks whether cfg.LaCale.CategoryID is set. If not, it
// fetches /api/external/meta, tries to auto-detect the TV-show category, and
// falls back to an interactive prompt when the result is ambiguous.
// On success it updates cfg in memory and rewrites the config file.
func ensureCategoryID(cfg *Config) error {
	if cfg.LaCale.CategoryID != "" {
		return nil
	}

	logf("category_id is not set — fetching categories from la-cale...")

	lc := newLaCaleClient(cfg.LaCale)
	meta, err := lc.GetMeta()
	if err != nil {
		return fmt.Errorf("fetching meta: %w", err)
	}

	// Flatten the category tree into a single list
	flat := flattenCategories(meta.Categories)
	if len(flat) == 0 {
		return fmt.Errorf("la-cale returned no categories")
	}

	// Keywords that suggest a TV-show category
	tvKeywords := []string{"serie", "tv", "show", "television", "emission", "drama"}

	var matches []Category
	for _, cat := range flat {
		key := strings.ToLower(cat.Name + " " + cat.Slug)
		for _, kw := range tvKeywords {
			if strings.Contains(key, kw) {
				matches = append(matches, cat)
				break
			}
		}
	}

	var chosen Category

	switch len(matches) {
	case 1:
		chosen = matches[0]
		logf("  Auto-detected TV category: %q (ID %s)", chosen.Name, chosen.ID)

	default:
		// 0 or many matches — ask the user
		candidates := matches
		if len(candidates) == 0 {
			candidates = flat
		}
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Name < candidates[j].Name
		})

		fmt.Println("\nAvailable categories (select the TV-show one):")
		for i, cat := range candidates {
			parent := ""
			if cat.ParentID != nil {
				parent = fmt.Sprintf(" (sous-cat.)")
			}
			fmt.Printf("  [%d] %s%s  —  id: %s\n", i+1, cat.Name, parent, cat.ID)
		}

		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Print("Enter number: ")
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line)
			n, err := strconv.Atoi(line)
			if err != nil || n < 1 || n > len(candidates) {
				fmt.Printf("Please enter a number between 1 and %d.\n", len(candidates))
				continue
			}
			chosen = candidates[n-1]
			break
		}
	}

	// Persist to config file
	if cfg.ConfigPath != "" {
		if err := updateConfigCategoryID(cfg.ConfigPath, chosen.ID); err != nil {
			logf("  warning: could not save category_id to config: %v", err)
		} else {
			logf("  Saved category_id = %q to %s", chosen.ID, cfg.ConfigPath)
		}
	}

	cfg.LaCale.CategoryID = chosen.ID
	return nil
}

// flattenCategories returns a flat slice of all categories including children.
func flattenCategories(cats []Category) []Category {
	var out []Category
	for _, c := range cats {
		out = append(out, c)
		if len(c.Children) > 0 {
			out = append(out, flattenCategories(c.Children)...)
		}
	}
	return out
}
