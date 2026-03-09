package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const tmdbBaseURL = "https://api.themoviedb.org/3"
const tmdbImageBase = "https://image.tmdb.org/t/p/w500"

// TMDBShow holds the fields we care about from a TMDB TV search result.
type TMDBShow struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	PosterPath  string `json:"poster_path"`
	FirstAirDate string `json:"first_air_date"`
	Overview    string `json:"overview"`
}

// TMDBSearchResponse represents the API response for /search/tv.
type TMDBSearchResponse struct {
	Page         int        `json:"page"`
	TotalResults int        `json:"total_results"`
	Results      []TMDBShow `json:"results"`
}

type tmdbClient struct {
	apiKey string
	http   *http.Client
}

func newTMDBClient(apiKey string) *tmdbClient {
	return &tmdbClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

// SearchTVShow searches for a TV show by title.
// Returns the best matching show, or an error if nothing was found.
func (c *tmdbClient) SearchTVShow(title string) (*TMDBShow, error) {
	endpoint := fmt.Sprintf("%s/search/tv", tmdbBaseURL)

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("api_key", c.apiKey)
	q.Set("query", title)
	q.Set("language", "fr-FR")
	req.URL.RawQuery = q.Encode()

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TMDB request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB API returned %d", resp.StatusCode)
	}

	var result TMDBSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding TMDB response: %w", err)
	}

	if len(result.Results) == 0 {
		return nil, fmt.Errorf("no TMDB results for %q", title)
	}

	return &result.Results[0], nil
}

// CoverURL returns the full cover image URL for a show poster path.
func CoverURL(posterPath string) string {
	if posterPath == "" {
		return ""
	}
	return tmdbImageBase + posterPath
}
