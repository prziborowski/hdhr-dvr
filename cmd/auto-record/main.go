package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	pkgcfg "github.com/prziborowski/hdhr-dvr/pkg/config"
	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

type RecordingRequest struct {
	ChannelID string  `json:"channelId"`
	Date      string  `json:"date"`
	StartTime string  `json:"startTime"`
	Duration  int     `json:"duration"`
	Title     *string `json:"title,omitempty"`
}

func main() {
	log.Println("Starting Auto-Record...")

	config, err := pkgcfg.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	db, err := initDB(config.StorageDir)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close() //nolint: errcheck

	// Load keywords from database
	keywords, err := loadKeywords(db)
	if err != nil {
		log.Fatalf("Failed to load keywords: %v", err)
	}

	if len(keywords) == 0 {
		log.Println("No keywords configured. Exiting.")
		return
	}

	log.Printf("Loaded %d keywords", len(keywords))

	// Load pending recordings to avoid duplicates
	pendingRecordings, err := loadPendingRecordings(db)
	if err != nil {
		log.Fatalf("Failed to load pending recordings: %v", err)
	}

	log.Printf("Found %d existing pending recordings", len(pendingRecordings))

	// Load guide data
	guideData, err := loadGuideData(config.GuideFile)
	if err != nil {
		log.Fatalf("Failed to load guide data: %v", err)
	}

	if len(guideData.Programs) == 0 {
		log.Println("No programs in guide. Exiting.")
		return
	}

	log.Printf("Loaded %d programs from guide", len(guideData.Programs))

	// Get local timezone for date calculations
	loc, err := time.LoadLocation(config.Timezone)
	if err != nil {
		log.Printf("Warning: Could not load timezone '%s', using UTC: %v", config.Timezone, err)
		loc = time.UTC
	}

	now := time.Now().In(loc)
	scheduledCount := 0

	// Check each program against keywords
	for _, program := range guideData.Programs {
		// Skip programs that have already ended
		endTime, err := parseProgramEndTime(program, loc)
		if err != nil {
			log.Printf("Warning: Could not parse end time for '%s': %v", program.Title, err)
			continue
		}

		if endTime.Before(now) {
			continue
		}

		// Check if this program matches any keywords
		matchedKeyword := findMatchingKeyword(program, keywords)
		if matchedKeyword == "" {
			continue
		}

		log.Printf("Found keyword match: '%s' in program '%s' (category: %s)",
			matchedKeyword, program.Title, program.Category)

		// Check if we already have a pending recording for this channel and time
		if isDuplicate(pendingRecordings, program) {
			log.Printf("Skipping duplicate: Already scheduled for channel %s at %s",
				program.Channel, program.Start)
			continue
		}

		// Calculate duration with sports bonus if applicable
		duration := calculateDuration(program)

		// Format date and time for the API request
		startTime, err := parseProgramStartTime(program, loc)
		if err != nil {
			log.Printf("Warning: Could not parse start time for '%s': %v", program.Title, err)
			continue
		}

		dateStr := startTime.Format("2006-01-02")
		timeStr := startTime.Format("15:04")

		title := program.Title
		if program.SubTitle != "" {
			title = fmt.Sprintf("%s - %s", title, program.SubTitle)
		}

		// Schedule the recording via API
		err = scheduleRecording(config.StorageDir+"/api/recordings", RecordingRequest{
			ChannelID: program.Channel,
			Date:      dateStr,
			StartTime: timeStr,
			Duration:  duration,
			Title:     &title,
		})

		if err != nil {
			log.Printf("Error scheduling recording for '%s': %v", title, err)
			continue
		}

		scheduledCount++
		log.Printf("Scheduled recording: %s on channel %s at %s (%d minutes)",
			title, program.Channel, timeStr, duration)

		// Add to pending recordings list to avoid duplicates
		pendingRecordings = append(pendingRecordings, types.Recording{
			ChannelID: program.Channel,
			Date:      dateStr,
			StartTime: timeStr,
		})
	}

	log.Printf("Auto-record complete. Scheduled %d new recordings.", scheduledCount)
}

func initDB(storageDir string) (*sql.DB, error) {
	dbPath := storageDir + "/recordings.db"
	return sql.Open("sqlite3", dbPath)
}

func loadKeywords(db *sql.DB) ([]types.Keyword, error) {
	rows, err := db.Query("SELECT id, name, category, enabled FROM keywords WHERE enabled = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint: errcheck

	var keywords []types.Keyword
	for rows.Next() {
		var k types.Keyword
		var category string
		if err := rows.Scan(&k.ID, &k.Name, &category, &k.Enabled); err != nil {
			return nil, err
		}
		k.Category = category
		keywords = append(keywords, k)
	}

	return keywords, rows.Err()
}

func loadPendingRecordings(db *sql.DB) ([]types.Recording, error) {
	rows, err := db.Query(`SELECT channel_id, date, start_time FROM recordings WHERE status = 'pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint: errcheck

	var recordings []types.Recording
	for rows.Next() {
		var r types.Recording
		if err := rows.Scan(&r.ChannelID, &r.Date, &r.StartTime); err != nil {
			return nil, err
		}
		recordings = append(recordings, r)
	}

	return recordings, rows.Err()
}

func loadGuideData(guideFile string) (*types.Guide, error) {
	data, err := os.ReadFile(guideFile)
	if err != nil {
		return nil, fmt.Errorf("reading guide file: %w", err)
	}

	var guide types.Guide
	if err := json.Unmarshal(data, &guide); err != nil {
		return nil, fmt.Errorf("parsing guide file: %w", err)
	}

	return &guide, nil
}

func findMatchingKeyword(program types.Program, keywords []types.Keyword) string {
	titleLower := strings.ToLower(program.Title)
	if program.SubTitle != "" {
		titleLower = strings.ToLower(program.Title + " " + program.SubTitle)
	}

	for _, keyword := range keywords {
		keywordLower := strings.ToLower(keyword.Name)

		// Check if keyword matches in title
		matchesInTitle := strings.Contains(titleLower, keywordLower)

		// If keyword has a category filter, check category match too
		if keyword.Category != "" && program.Category != "" {
			categoryMatch := strings.EqualFold(keyword.Category, program.Category)
			if !categoryMatch {
				continue
			}
		}

		if matchesInTitle {
			return keyword.Name
		}
	}

	return ""
}

func isDuplicate(pendingRecordings []types.Recording, program types.Program) bool {
	startTime, err := parseProgramStartTime(program, time.UTC)
	if err != nil {
		return false
	}

	for _, rec := range pendingRecordings {
		recTime, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%s %s", rec.Date, rec.StartTime), time.UTC)
		if err != nil {
			continue
		}

		// Check if same channel and within 30 minutes of each other
		if strings.EqualFold(rec.ChannelID, program.Channel) {
			diff := startTime.Sub(recTime).Minutes()
			if diff >= -30 && diff <= 30 {
				return true
			}
		}
	}

	return false
}

func calculateDuration(program types.Program) int {
	duration := program.Duration

	// Add extra time for sports events with basketball, football, or fifa in title
	if strings.EqualFold(program.Category, "sports") {
		titleLower := strings.ToLower(program.Title)
		if program.SubTitle != "" {
			titleLower = strings.ToLower(program.Title + " " + program.SubTitle)
		}

		if strings.Contains(titleLower, "basketball") ||
			strings.Contains(titleLower, "football") ||
			strings.Contains(titleLower, "fifa") {
			duration += 15 // Add 15 minutes
		}
	}

	return duration
}

func parseProgramStartTime(program types.Program, loc *time.Location) (time.Time, error) {
	startTimeStr := program.Start
	if strings.HasSuffix(startTimeStr, "Z") {
		startTimeStr = strings.TrimSuffix(startTimeStr, "Z")
	}

	t, err := time.Parse("2006-01-02T15:04:05", startTimeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing start time: %w", err)
	}

	return t.In(loc), nil
}

func parseProgramEndTime(program types.Program, loc *time.Location) (time.Time, error) {
	endTimeStr := program.End
	if strings.HasSuffix(endTimeStr, "Z") {
		endTimeStr = strings.TrimSuffix(endTimeStr, "Z")
	}

	t, err := time.Parse("2006-01-02T15:04:05", endTimeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing end time: %w", err)
	}

	return t.In(loc), nil
}

func scheduleRecording(apiURL string, req RecordingRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	resp, err := http.Post(
		apiURL,
		"application/json",
		bytes.NewBuffer(body),
	)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close() //nolint: errcheck

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var errMsg map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&errMsg); err == nil {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, errMsg["error"])
		}
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
