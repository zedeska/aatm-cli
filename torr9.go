package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const torr9BaseURL = "https://api.torr9.net"

// torr9Client wraps the Torr9 REST API.
type torr9Client struct {
	cfg  Torr9Config
	http *http.Client
}

func newTorr9Client(cfg Torr9Config) *torr9Client {
	return &torr9Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *torr9Client) setAuth(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
}

// ── Generate description ──────────────────────────────────────────────────────

// Torr9TechInfo holds the technical details submitted to generate-description.
type Torr9TechInfo struct {
	Source       string `json:"source"`
	Resolution   string `json:"resolution"`
	VideoFormat  string `json:"video_format"`
	VideoCodec   string `json:"video_codec"`
	VideoBitrate string `json:"video_bitrate"`
	Audio        string `json:"audio"`
	Subtitles    string `json:"subtitles"`
}

// Torr9GenerateDescRequest is the body for POST /api/v1/torrents/generate-description.
type Torr9GenerateDescRequest struct {
	Title       string        `json:"title"`
	NFO         string        `json:"nfo"`
	Category    string        `json:"category"`
	TmdbID      int           `json:"tmdb_id"`
	ReleaseName string        `json:"release_name"`
	SizeBytes   int64         `json:"size_bytes"`
	FileCount   int           `json:"file_count"`
	TechInfo    Torr9TechInfo `json:"tech_info"`
}

// Torr9GenerateDescResponse is the response from generate-description.
type Torr9GenerateDescResponse struct {
	Description string        `json:"description"`
	Metadata    Torr9Metadata `json:"metadata"`
	NFOData     Torr9NFOData  `json:"nfo_data"`
}

// Torr9Metadata holds the show metadata returned by generate-description.
type Torr9Metadata struct {
	BackdropURL string   `json:"backdrop_url"`
	Genres      []string `json:"genres"`
	ImdbID      string   `json:"imdb_id"`
	PosterURL   string   `json:"poster_url"`
	Rating      float64  `json:"rating"`
	ReleaseYear int      `json:"release_year"`
	Synopsis    string   `json:"synopsis"`
	Title       string   `json:"title"`
	TmdbID      int      `json:"tmdb_id"`
	TvdbID      int      `json:"tvdb_id"`
}

// Torr9NFOData holds the parsed tech info echoed back by generate-description.
type Torr9NFOData struct {
	Bitrate    int    `json:"bitrate"`
	Resolution string `json:"resolution"`
	Source     string `json:"source"`
	VideoCodec string `json:"video_codec"`
}

// GenerateDescription calls POST /api/v1/torrents/generate-description.
func (c *torr9Client) GenerateDescription(req Torr9GenerateDescRequest) (*Torr9GenerateDescResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, torr9BaseURL+"/api/v1/torrents/generate-description", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuth(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("torr9 generate-description failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("torr9 generate-description returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result Torr9GenerateDescResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding generate-description response: %w", err)
	}
	return &result, nil
}

// ── Check duplicate ───────────────────────────────────────────────────────────

// Torr9CheckDupRequest is the body for POST /api/v1/torrents/check-duplicate.
type Torr9CheckDupRequest struct {
	Title string `json:"title"`
}

// Torr9CheckDupResponse is the response from check-duplicate.
type Torr9CheckDupResponse struct {
	IsDuplicate bool            `json:"is_duplicate"`
	Matches     []Torr9DupMatch `json:"matches"`
}

// Torr9DupMatch holds a matched release returned by check-duplicate.
type Torr9DupMatch struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// CheckDuplicate calls POST /api/v1/torrents/check-duplicate.
func (c *torr9Client) CheckDuplicate(title string) (*Torr9CheckDupResponse, error) {
	reqBody, err := json.Marshal(Torr9CheckDupRequest{Title: title})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, torr9BaseURL+"/api/v1/torrents/check-duplicate", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuth(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("torr9 check-duplicate failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("torr9 check-duplicate returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result Torr9CheckDupResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding check-duplicate response: %w", err)
	}
	return &result, nil
}

// ── Upload ────────────────────────────────────────────────────────────────────

// Torr9UploadRequest holds all fields needed for POST /api/v1/torrents/upload.
type Torr9UploadRequest struct {
	TorrentPath string
	Title       string
	Description string
	NFO         string
	Category    string
	Subcategory string
	Tags        string
	IsExclusive bool
	IsAnonymous bool
	TmdbID      int
	ImdbID      string
	TvdbID      int
}

// Torr9UploadResponse is the response from the upload endpoint.
type Torr9UploadResponse struct {
	AnnounceURL string `json:"announce_url"`
	DownloadURL string `json:"download_url"`
	InfoHash    string `json:"info_hash"`
	MagnetLink  string `json:"magnet_link"`
	Message     string `json:"message"`
	Status      string `json:"status"`
	TorrentFile string `json:"torrent_file"`
	TorrentID   int    `json:"torrent_id"`
}

// Upload calls POST /api/v1/torrents/upload (multipart/form-data).
func (c *torr9Client) Upload(req Torr9UploadRequest) (*Torr9UploadResponse, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	// Attach .torrent file first
	f, err := os.Open(req.TorrentPath)
	if err != nil {
		return nil, fmt.Errorf("opening torrent file: %w", err)
	}
	part, err := mw.CreateFormFile("torrent_file", filepath.Base(req.TorrentPath))
	if err != nil {
		f.Close()
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()

	wf := func(key, value string) error { return mw.WriteField(key, value) }

	if err := wf("title", req.Title); err != nil {
		return nil, err
	}
	if err := wf("description", req.Description); err != nil {
		return nil, err
	}
	if err := wf("nfo", req.NFO); err != nil {
		return nil, err
	}
	if err := wf("category", req.Category); err != nil {
		return nil, err
	}
	if req.Subcategory != "" {
		if err := wf("subcategory", req.Subcategory); err != nil {
			return nil, err
		}
	}
	if err := wf("tags", req.Tags); err != nil {
		return nil, err
	}
	if err := wf("is_exclusive", boolStr(req.IsExclusive)); err != nil {
		return nil, err
	}
	if err := wf("is_anonymous", boolStr(req.IsAnonymous)); err != nil {
		return nil, err
	}
	if req.TmdbID != 0 {
		if err := wf("tmdb_id", fmt.Sprintf("%d", req.TmdbID)); err != nil {
			return nil, err
		}
	}
	if req.ImdbID != "" {
		if err := wf("imdb_id", req.ImdbID); err != nil {
			return nil, err
		}
	}
	if req.TvdbID != 0 {
		if err := wf("tvdb_id", fmt.Sprintf("%d", req.TvdbID)); err != nil {
			return nil, err
		}
	}

	if err := mw.Close(); err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, torr9BaseURL+"/api/v1/torrents/upload", &body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	c.setAuth(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("torr9 upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("torr9 upload returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result Torr9UploadResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding upload response: %w", err)
	}
	return &result, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
