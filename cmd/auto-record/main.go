package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

// APIResponseRecording matches the JSON structure returned by /api/recordings
type APIResponseRecording struct {
	ID          int     `json:"id"`
	ChannelID   string  `json:"channel_id"`
	Date        string  `json:"date"`
	StartTime   string  `json:"start_time"`
	Duration    int     `json:"duration"`
	Status      string  `json:"status"`
	Title       *string `json:"title,omitempty"`
	GuideNumber string  `json:"guide_number"`
	GuideName   string  `json:"guide_name"`
}

func main() {
	log.Println("Starting Auto-Record...")

	config, err := pkgcfg.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	apiBaseURL := "http://localhost:8080"

	// Load keywords via API
	keywords, err := fetchKeywords(apiBaseURL)
	if err != nil {
		log.Fatalf("Failed to load keywords: %v", err)
	}

	if len(keywords) == 0 {
		log.Println("No keywords configured. Exiting.")
		return
	}

	log.Printf("Loaded %d keywords", len(keywords))

	// Load pending recordings via API to avoid duplicates
	pendingRecordings, err := fetchPendingRecordings(apiBaseURL)
	if err != nil {
		log.Fatalf("Failed to load pending recordings: %v", err)
	}

	log.Printf("Found %d existing pending recordings", len(pendingRecordings))

	// Load guide data
	guideData, err := loadGuideData(apiBaseURL)
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

		title := program.Title
		if program.SubTitle != "" {
			title = fmt.Sprintf("%s - %s", title, program.SubTitle)
		}

		// Check if we already have a pending recording for this channel and time
		if rec := findPendingRecording(pendingRecordings, program, loc); rec != nil {
			log.Printf("Found existing recording for channel %s at %s", program.Channel, program.Start)

			existingTitle := ""
			if rec.Title != nil {
				existingTitle = *rec.Title
			}

			if title != existingTitle {
				log.Printf("Updating title from '%s' to '%s'", existingTitle, title)
				if err := updateRecordingTitle(apiBaseURL, rec.ID, title); err != nil {
					log.Printf("Error updating recording title: %v", err)
				}
			} else {
				log.Printf("Skipping duplicate: Already scheduled with same title")
			}
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

		// Schedule the recording via API
		apiURL := apiBaseURL + "/api/recordings"
		err = scheduleRecording(apiURL, RecordingRequest{
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

		// Add to pending recordings list to avoid duplicates in the same run
		pendingRecordings = append(pendingRecordings, types.Recording{
			ChannelID: program.Channel,
			Date:      dateStr,
			StartTime: timeStr,
		})
	}

	log.Printf("Auto-record complete. Scheduled %d new recordings.", scheduledCount)
}

func fetchKeywords(baseURL string) ([]types.Keyword, error) {
	return fetchJSONWithRetry[types.Keyword](baseURL, "/api/keywords", 3)
}

func fetchPendingRecordings(baseURL string) ([]types.Recording, error) {
	type pendingRecording struct {
		ID        int     `json:"id"`
		ChannelID string  `json:"channel_id"`
		Date      string  `json:"date"`
		StartTime string  `json:"start_time"`
		Duration  int     `json:"duration"`
		Status    string  `json:"status"`
		Title     *string `json:"title,omitempty"`
	}

	raw, err := fetchJSONWithRetry[pendingRecording](baseURL, "/api/recordings", 3)
	if err != nil {
		return nil, err
	}

	var pending []types.Recording
	for _, r := range raw {
		if r.Status == "pending" {
			pending = append(pending, types.Recording{
				ID:        r.ID,
				ChannelID: r.ChannelID,
				Date:      r.Date,
				StartTime: r.StartTime,
				Duration:  r.Duration,
				Status:    r.Status,
				Title:     r.Title,
			})
		}
	}

	return pending, nil
}

func fetchJSONWithRetry[T any](baseURL, path string, maxRetries int) ([]T, error) {
	url := baseURL + path
	var result []T
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			log.Printf("Retrying %s (attempt %d/%d) in %v... (%v)", url, attempt+1, maxRetries+1, backoff, lastErr)
			time.Sleep(backoff)
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			lastErr = fmt.Errorf("requesting %s: %w", url, err)
			continue
		}

		defer resp.Body.Close() //nolint: errcheck

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("unexpected status code fetching %s: %d", url, resp.StatusCode)
			continue
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = fmt.Errorf("decoding %s: %w", url, err)
			continue
		}

		return result, nil
	}

	return nil, lastErr
}

func loadGuideData(apiBaseURL string) (*types.Guide, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(apiBaseURL + "/api/guide")
	if err != nil {
		return nil, fmt.Errorf("fetching guide data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var guide types.Guide
	if err := json.NewDecoder(resp.Body).Decode(&guide); err != nil {
		return nil, fmt.Errorf("decoding guide data: %w", err)
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
		if !matchesInTitle {
			continue
		}

		// If keyword has a category filter, it MUST match the program's category
		if keyword.Category != "" {
			if program.Category == "" || !strings.EqualFold(keyword.Category, program.Category) {
				continue
			}
		}

		return keyword.Name
	}

	return ""
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
			strings.Contains(titleLower, "football") {
			duration += 15 // Add 15 minutes
		}
	}

	return duration
}

func parseProgramStartTime(program types.Program, loc *time.Location) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, program.Start)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing start time: %w", err)
	}

	return t.In(loc), nil
}

func parseProgramEndTime(program types.Program, loc *time.Location) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, program.End)
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

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(
		apiURL,
		"application/json",
		bytes.NewBuffer(body),
	)
	if err != nil {
		return fmt.Errorf("sending request to %s: %w", apiURL, err)
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
func findPendingRecording(pendingRecordings []types.Recording, program types.Program, loc *time.Location) *types.Recording {
	startTimeUTC, err := parseProgramStartTime(program, time.UTC)
	if err != nil {
		return nil
	}

	for _, rec := range pendingRecordings {
		recTimeLocal, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%s %s", rec.Date, rec.StartTime), loc)
		if err != nil {
			continue
		}
		recTimeUTC := recTimeLocal.UTC()

		if strings.EqualFold(rec.ChannelID, program.Channel) {
			diff := startTimeUTC.Sub(recTimeUTC).Minutes()
			if diff >= -30 && diff <= 30 {
				return &rec
			}
		}
	}
	return nil
}

func updateRecordingTitle(apiURL string, id int, title string) error {
	reqBody := map[string]string{"title": title}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	url := fmt.Sprintf("%s/api/recordings/%d", apiURL, id)
	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
