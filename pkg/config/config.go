package config

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
)

const DefaultAPIBaseURL = "http://localhost:8080"

// APIBaseURL returns the app API base URL from the API_BASE_URL env var,
// falling back to DefaultAPIBaseURL. Trims surrounding whitespace and a
// trailing slash, and validates the result is a parseable URL with a host.
func APIBaseURL() (string, error) {
	v := strings.TrimRight(strings.TrimSpace(os.Getenv("API_BASE_URL")), "/")
	if v == "" {
		return DefaultAPIBaseURL, nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid API_BASE_URL %q: %w", v, err)
	}
	return v, nil
}

type Config struct {
	Timezone   string `json:"timezone"`
	UserID     string `json:"userId"`
	LineUpID   string `json:"lineUpID"`
	Days       int    `json:"days"`
	StorageDir string `json:"storageDir"`
}

// LoadConfig reads the configuration from config.json
func LoadConfig() (*Config, error) {
	var config Config

	file, err := os.ReadFile("config.json")
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(file, &config); err != nil {
		log.Printf("Failed to unmarshal config file: %v", err)
		return nil, err
	}

	if config.Timezone == "" {
		config.Timezone = "America/Los_Angeles"
		log.Println("WARNING: timezone not set, defaulting to America/Los_Angeles")
	}
	if config.Days == 0 || config.Days > 8 {
		log.Printf("WARNING: days=%d is invalid, clamping to 8", config.Days)
		config.Days = 8
	}

	if config.StorageDir == "" {
		log.Fatalf("storageDir cannot be unset")
	}

	return &config, nil
}
