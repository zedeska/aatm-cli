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
	cfg  QBittorrentConfig
	http *http.Client
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

// AddTorrent uploads a .torrent file to qBittorrent.
// On a 403 (expired session) it re-authenticates and retries once.
func (c *qbittorrentClient) AddTorrent(torrentPath, infoHash string) error {
	err := c.addTorrentOnce(torrentPath)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "403") {
		return err
	}
	// Session expired — re-login and retry.
	logf("  qBittorrent session expired, re-logging in...")
	if loginErr := c.Login(); loginErr != nil {
		return fmt.Errorf("re-login failed: %w", loginErr)
	}
	return c.addTorrentOnce(torrentPath)
}

func (c *qbittorrentClient) addTorrentOnce(torrentPath string) error {
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

	// Enable auto-management so qBittorrent honours the global seeding action
	if err := mw.WriteField("autoTMM", "false"); err != nil {
		return err
	}
	// Skip initial hash check — file is already in place, start seeding immediately.
	if err := mw.WriteField("skip_checking", "true"); err != nil {
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
	req.Header.Set("Origin", c.cfg.Host)
	req.Header.Set("Referer", c.cfg.Host)

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

	req, err := http.NewRequest(http.MethodPost, c.cfg.Host+"/api/v2/torrents/setShareLimits", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("setShareLimits failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", c.cfg.Host)
	req.Header.Set("Referer", c.cfg.Host)

	resp, err := c.http.Do(req)
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

// SetSuperSeeding enables super-seeding (initial seeding) on an already-added torrent.
func (c *qbittorrentClient) SetSuperSeeding(infoHash string) error {
	form := url.Values{}
	form.Set("hashes", infoHash)
	form.Set("value", "true")

	req, err := http.NewRequest(http.MethodPost, c.cfg.Host+"/api/v2/torrents/setSuperSeeding", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", c.cfg.Host)
	req.Header.Set("Referer", c.cfg.Host)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("setSuperSeeding returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// Reannounce forces an immediate re-announce to all trackers for the torrent.
func (c *qbittorrentClient) Reannounce(infoHash string) error {
	form := url.Values{}
	form.Set("hashes", infoHash)

	req, err := http.NewRequest(http.MethodPost, c.cfg.Host+"/api/v2/torrents/reannounce", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", c.cfg.Host)
	req.Header.Set("Referer", c.cfg.Host)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("reannounce returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// AddAndConfigureTorrent is a convenience wrapper: adds the torrent, applies the ratio limit,
// enables super-seeding, and forces a re-announce.
func (c *qbittorrentClient) AddAndConfigureTorrent(torrentPath, infoHash string, ratioLimit float64) error {
	if err := c.AddTorrent(torrentPath, infoHash); err != nil {
		return err
	}

	// qBittorrent needs a moment before it accepts post-add API calls for a new hash.
	time.Sleep(2 * time.Second)

	if err := c.SetShareLimits(infoHash, ratioLimit); err != nil {
		fmt.Printf("warning: could not set ratio limit: %v\n", err)
	}
	if err := c.SetSuperSeeding(infoHash); err != nil {
		fmt.Printf("warning: could not enable super-seeding: %v\n", err)
	}
	if err := c.Reannounce(infoHash); err != nil {
		fmt.Printf("warning: could not reannounce: %v\n", err)
	}
	return nil
}
