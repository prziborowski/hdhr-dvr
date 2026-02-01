package config

import (
	"encoding/json"
	"log"
	"os"
)

type Config struct {
	Timezone      string `json:"timezone"`
	LineUpID      string `json:"lineUpID"`
	Days          int    `json:"days"`
	GuideFile     string `json:"guideFile"`
	StateFile     string `json:"stateFile"`
	DefaultConfig bool
}

// LoadConfig reads the configuration from config.json
func LoadConfig() *Config {
	var config Config

	file, err := os.ReadFile("config.json")
	if err != nil {
		log.Printf("Failed to read config file: %v", err)
		config.DefaultConfig = true
	} else {
		if err := json.Unmarshal(file, &config); err != nil {
			log.Printf("Failed to unmarshal config file: %v", err)
			config.DefaultConfig = true
		}
	}

	if config.Timezone == "" {
		config.Timezone = "America/Los_Angeles"
	}
	if config.Days == 0 || config.Days > 8 {
		config.Days = 8
	}
	if config.GuideFile == "" {
		config.GuideFile = "guide.json"
	}
	if config.StateFile == "" {
		config.StateFile = "guide_state.json"
	}

	return &config
}
