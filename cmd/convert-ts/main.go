package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// apiRecording is the minimal subset of /api/recordings this client needs.
type apiRecording struct {
	ID        int    `json:"id"`
	ChannelID string `json:"channel_id"`
	Date      string `json:"date"`
	StartTime string `json:"start_time"`
	Duration  int    `json:"duration"`
	Status    string `json:"status"`
	Title     string `json:"title"`
}

// convertResponse matches the JSON returned by POST /api/recordings/{id}/convert.
type convertResponse struct {
	ID        int    `json:"id"`
	Converted bool   `json:"converted"`
	Reason    string `json:"reason"`
	FileSize  int64  `json:"file_size"`
}

func main() {
	log.Println("Starting convert-ts recovery...")

	apiBaseURL := "http://localhost:8080"

	recordings, err := fetchRecordings(apiBaseURL)
	if err != nil {
		log.Fatalf("Failed to load recordings: %v", err)
	}

	log.Printf("Found %d recordings", len(recordings))

	// The convert endpoint is synchronous and can take a long time on large
	// files, so use a very long HTTP timeout.
	client := &http.Client{Timeout: 30 * time.Minute}

	var converted, skipped, failed int
	var failedIDs []int

	for _, rec := range recordings {
		if rec.Status != "completed" {
			continue
		}

		res, err := convertRecording(client, apiBaseURL, rec.ID)
		if err != nil {
			failed++
			failedIDs = append(failedIDs, rec.ID)
			log.Printf("Failed to convert recording %d: %v", rec.ID, err)
			continue
		}

		if res.Converted {
			converted++
			log.Printf("Converted recording %d (%s) to MP4 (%d bytes)", res.ID, rec.Title, res.FileSize)
		} else {
			skipped++
			log.Printf("Skipped recording %d (%s): %s", res.ID, rec.Title, res.Reason)
		}
	}

	log.Printf("convert-ts complete: %d converted, %d skipped, %d failed", converted, skipped, failed)
	if len(failedIDs) > 0 {
		log.Printf("Failed IDs: %v", failedIDs)
	}
}

func fetchRecordings(baseURL string) ([]apiRecording, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(baseURL + "/api/recordings")
	if err != nil {
		return nil, fmt.Errorf("requesting recordings: %w", err)
	}
	defer resp.Body.Close() //nolint: errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code fetching recordings: %d", resp.StatusCode)
	}

	var recordings []apiRecording
	if err := json.NewDecoder(resp.Body).Decode(&recordings); err != nil {
		return nil, fmt.Errorf("decoding recordings: %w", err)
	}
	return recordings, nil
}

func convertRecording(client *http.Client, baseURL string, id int) (*convertResponse, error) {
	url := fmt.Sprintf("%s/api/recordings/%d/convert", baseURL, id)
	resp, err := client.Post(url, "application/json", nil)
	if err != nil {
		return nil, fmt.Errorf("requesting conversion for %d: %w", id, err)
	}
	defer resp.Body.Close() //nolint: errcheck

	if resp.StatusCode == http.StatusConflict {
		// A 409 means the recording is not convertible right now (not completed
		// or no .ts on disk). This is a benign no-op, not a failure.
		var res convertResponse
		_ = json.NewDecoder(resp.Body).Decode(&res)
		if res.Reason == "" {
			res.Reason = "conflict"
		}
		return &res, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("conversion for %d returned status %d", id, resp.StatusCode)
	}

	var res convertResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decoding conversion response for %d: %w", id, err)
	}
	return &res, nil
}
