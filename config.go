package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	LaCale      LaCaleConfig      `toml:"lacale"`
	Torr9       Torr9Config       `toml:"torr9"`
	TMDB        TMDBConfig        `toml:"tmdb"`
	Torrent     TorrentConfig     `toml:"torrent"`
	QBittorrent QBittorrentConfig `toml:"qbittorrent"`

	// ConfigPath holds the resolved filesystem path of the loaded config file.
	// Not part of the TOML schema — used internally for in-place updates.
	ConfigPath string `toml:"-"`
}

type Torr9Config struct {
	APIKey             string `toml:"api_key"`
	Category           string `toml:"category"`
	Subcategory        string `toml:"subcategory"`
	SkipDuplicateCheck bool   `toml:"skip_duplicate_check"`
}

type LaCaleConfig struct {
	APIKey             string `toml:"api_key"`
	BaseURL            string `toml:"base_url"`
	CategoryID         string `toml:"category_id"`
	SkipDuplicateCheck bool   `toml:"skip_duplicate_check"`
}

type TMDBConfig struct {
	APIKey string `toml:"api_key"`
}

type TorrentConfig struct {
	Trackers []string `toml:"trackers"`
}

type QBittorrentConfig struct {
	Host          string  `toml:"host"`
	Username      string  `toml:"username"`
	Password      string  `toml:"password"`
	SavePath      string  `toml:"save_path"`
	LocalSavePath string  `toml:"local_save_path"`
	RatioLimit    float64 `toml:"ratio_limit"`
}

func resolveConfigPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine config dir: %w", err)
	}
	return filepath.Join(cfgDir, "aatm-cli", "config.toml"), nil
}

func loadConfig(path string) (*Config, error) {
	resolved, err := resolveConfigPath(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if _, err := toml.DecodeFile(resolved, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load config from %s: %w", resolved, err)
	}

	if cfg.QBittorrent.RatioLimit == 0 {
		cfg.QBittorrent.RatioLimit = 2.0
	}
	cfg.ConfigPath = resolved

	return &cfg, nil
}

// updateConfigCategoryID rewrites the category_id value in the [lacale] section
// of the config file in-place, preserving all comments and other fields.
func updateConfigCategoryID(configPath, categoryID string) error {
	f, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("opening config: %w", err)
	}

	var lines []string
	inLaCale := false
	replaced := false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Track which section we're in
		if strings.HasPrefix(trimmed, "[") {
			inLaCale = trimmed == "[lacale]"
		}

		if inLaCale && !replaced {
			// Replace existing key
			lower := strings.ToLower(trimmed)
			if strings.HasPrefix(lower, "category_id") && strings.Contains(lower, "=") {
				line = fmt.Sprintf(`category_id = "%s"`, categoryID)
				replaced = true
			}
		}

		lines = append(lines, line)
	}
	f.Close()

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	// If the key was absent from [lacale], append it after the [lacale] header
	if !replaced {
		var out []string
		inLaCale = false
		injected := false
		for _, line := range lines {
			out = append(out, line)
			trimmed := strings.TrimSpace(line)
			if !injected {
				if strings.HasPrefix(trimmed, "[") {
					inLaCale = trimmed == "[lacale]"
				} else if inLaCale && (strings.HasPrefix(trimmed, "api_key") || strings.HasPrefix(trimmed, "base_url")) {
					// Inject after a known lacale key
					out = append(out, fmt.Sprintf(`category_id = "%s"`, categoryID))
					injected = true
				}
			}
		}
		lines = out
	}

	content := strings.Join(lines, "\n")
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}
