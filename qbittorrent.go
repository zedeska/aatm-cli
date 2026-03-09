package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// qbittorrentClient wraps the qBittorrent Web API v2.
type qbittorrentClient struct {
	cfg    QBittorrentConfig
	http   *http.Client
}

func newQBittorrentClient(cfg QBittorrentConfig) (*qbittorrentClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &qbittorrentClient{
		cfg: cfg,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
		},
	}, nil
}

// Login authenticates with the qBittorrent Web API and stores the session cookie.
func (c *qbittorrentClient) Login() error {
	form := url.Values{}
	form.Set("username", c.cfg.Username)
	form.Set("password", c.cfg.Password)

	resp, err := c.http.PostForm(c.cfg.Host+"/api/v2/auth/login", form)
	if err != nil {
		return fmt.Errorf("qBittorrent login failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) == "Fails." {
		return fmt.Errorf("qBittorrent login: wrong credentials")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qBittorrent login returned %d", resp.StatusCode)
	}
	return nil
}

// AddTorrent uploads a .torrent file to qBittorrent with the configured save path.
// Returns the infohash so we can later set share limits.
func (c *qbittorrentClient) AddTorrent(torrentPath, infoHash, savePath string) error {
	f, err := os.Open(torrentPath)
	if err != nil {
		return fmt.Errorf("opening torrent: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	part, err := mw.CreateFormFile("torrents", filepath.Base(torrentPath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}

	if savePath != "" {
		if err := mw.WriteField("savepath", savePath); err != nil {
			return err
		}
	}

	// Enable auto-management so qBittorrent honours the global seeding action
	if err := mw.WriteField("autoTMM", "false"); err != nil {
		return err
	}

	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.cfg.Host+"/api/v2/torrents/add", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qBittorrent add torrent failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qBittorrent add returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SetShareLimits sets the ratio limit on an already-added torrent.
// ratioLimit: -2 = global, -1 = no limit, ≥0 = specific value.
// seedingTimeLimit: -2 = global, -1 = no limit.
func (c *qbittorrentClient) SetShareLimits(infoHash string, ratioLimit float64) error {
	form := url.Values{}
	form.Set("hashes", infoHash)
	form.Set("ratioLimit", fmt.Sprintf("%.2f", ratioLimit))
	form.Set("seedingTimeLimit", "-2")         // use global
	form.Set("inactiveSeedingTimeLimit", "-2") // use global

	resp, err := c.http.PostForm(c.cfg.Host+"/api/v2/torrents/setShareLimits", form)
	if err != nil {
		return fmt.Errorf("setShareLimits failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("setShareLimits returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// AddAndConfigureTorrent is a convenience wrapper: adds the torrent then applies the ratio limit.
func (c *qbittorrentClient) AddAndConfigureTorrent(torrentPath, infoHash, savePath string, ratioLimit float64) error {
	if err := c.AddTorrent(torrentPath, infoHash, savePath); err != nil {
		return err
	}

	// qBittorrent needs a moment before it accepts setShareLimits for a new hash
	time.Sleep(2 * time.Second)

	if err := c.SetShareLimits(infoHash, ratioLimit); err != nil {
		// non-fatal – log warning but don't abort
		fmt.Printf("warning: could not set ratio limit: %v\n", err)
	}
	return nil
}
