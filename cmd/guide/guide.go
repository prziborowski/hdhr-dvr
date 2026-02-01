package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	pkgcfg "github.com/prziborowski/hdhr-dvr/pkg/config"
	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

func fetchLocalChannels() ([]types.Channel, error) {
	resp, err := http.Get("http://localhost:8080/api/channels")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint: errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var channels []types.Channel
	if err := json.Unmarshal(body, &channels); err != nil {
		return nil, err
	}

	return channels, nil
}

func fetchLineupData(url string) ([]types.LineupData, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint: errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data []types.LineupData
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	return data, nil
}

func fetchListingData(url string) ([][]types.ListingData, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint: errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data [][]types.ListingData
	if err := json.Unmarshal(body, &data); err != nil {
		// Try to get more context about the error
		log.Printf("Raw response: %s", string(body[:min(100, len(body))]))
		return nil, err
	}

	return data, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	// Load configuration
	config := pkgcfg.LoadConfig()
	if config.DefaultConfig {
		log.Fatalf("Generating guide requires config.json configuration")
	}

	log.Printf("Using configuration: timezone=%s, lineUpID=%s, days=%d",
		config.Timezone, config.LineUpID, config.Days)

	// Load previous state if it exists
	var processedDays map[string]bool
	stateData, err := os.ReadFile(config.StateFile)
	if err == nil {
		json.Unmarshal(stateData, &processedDays) //nolint: errcheck
	} else if !os.IsNotExist(err) {
		log.Printf("Error reading state file: %v", err)
	}
	if processedDays == nil {
		processedDays = make(map[string]bool)
	}

	// Load existing guide data if it exists
	var existingGuide types.Guide
	existingData, err := os.ReadFile(config.GuideFile)
	if err == nil {
		if err := json.Unmarshal(existingData, &existingGuide); err != nil {
			log.Printf("Error parsing existing guide: %v", err)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("Error reading existing guide: %v", err)
	}

	// Get local channels we can receive
	var (
		localChannels       []types.Channel
		filteredLineup      []types.LineupData
		useExistingChannels bool
	)

	// Check if we have existing channel data
	if len(existingGuide.Channels) > 0 {
		useExistingChannels = true
		filteredLineup = existingGuide.Channels
		log.Printf("Using existing channel data (%d channels)", len(filteredLineup))
	}

	// Only fetch local channels if we don't have existing channel data
	if !useExistingChannels {
		log.Println("Fetching local channels...")
		localChannels, err = fetchLocalChannels()
		if err != nil {
			log.Printf("Warning: Could not fetch local channels: %v", err)
			log.Println("Will use all available channels from lineup")
		} else {
			// GET lineup data from tvtv.us
			lineupURL := fmt.Sprintf("https://www.tvtv.us/api/v1/lineup/%s/channels", config.LineUpID)
			lineupData, err := fetchLineupData(lineupURL)
			if err != nil {
				log.Fatalf("Error fetching lineup data: %v", err)
			}

			// Filter lineup to only include channels we can receive
			for _, channel := range lineupData {
				for _, local := range localChannels {
					if channel.ChannelNumber == local.GuideNumber {
						filteredLineup = append(filteredLineup, channel)
						break
					}
				}
			}
		}
	}

	// If we still don't have any channels, get all available channels
	if len(filteredLineup) == 0 {
		log.Println("No channel filtering applied, using all available channels")
		lineupURL := fmt.Sprintf("https://www.tvtv.us/api/v1/lineup/%s/channels", config.LineUpID)
		lineupData, err := fetchLineupData(lineupURL)
		if err != nil {
			log.Fatalf("Error fetching lineup data: %v", err)
		}
		filteredLineup = lineupData
	}

	// Process channels
	allChannels := make([]string, 0, len(filteredLineup))
	for _, channel := range filteredLineup {
		allChannels = append(allChannels, channel.StationID)
	}

	var newPrograms []types.Program

	// Process each day
	for day := 0; day < config.Days; day++ {
		dayKey := time.Now().Add(time.Duration(day) * 24 * time.Hour).Format("2006-01-02")

		// Skip if we've already processed this day
		if processedDays[dayKey] {
			log.Printf("Skipping already processed day: %s", dayKey)
			continue
		}

		// Calculate day boundaries in local time
		loc, err := time.LoadLocation(config.Timezone)
		if err != nil {
			log.Printf("Error loading timezone: %v", err)
			continue
		}

		// Calculate midnight in local time
		now := time.Now().In(loc)
		midnightLocal := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		midnightLocal = midnightLocal.Add(time.Duration(day) * 24 * time.Hour)

		// Convert to UTC
		midnightUTC := midnightLocal.UTC()
		endOfDayLocal := midnightLocal.Add(24*time.Hour - time.Second)
		endOfDayUTC := endOfDayLocal.UTC()

		// Format as ISO 8601 with milliseconds
		startTime := midnightUTC.Format("2006-01-02T15:04:05.000Z")
		endTime := endOfDayUTC.Format("2006-01-02T15:04:05.000Z")

		log.Printf("Fetching data for day %s (Local: %s to %s, UTC: %s to %s)",
			dayKey,
			midnightLocal.Format("2006-01-02T15:04:05-07:00"),
			endOfDayLocal.Format("2006-01-02T15:04:05-07:00"),
			startTime,
			endTime)

		// Load listing data in batches of 20 channels max
		var listingData [][]types.ListingData
		for i := 0; i < len(allChannels); i += 20 {
			end := i + 20
			if end > len(allChannels) {
				end = len(allChannels)
			}
			channels := allChannels[i:end]
			listingURL := fmt.Sprintf("https://www.tvtv.us/api/v1/lineup/%s/grid/%s/%s/%s",
				config.LineUpID, startTime, endTime, strings.Join(channels, ","))
			batchData, err := fetchListingData(listingURL)
			if err != nil {
				log.Printf("Error fetching listing data for day %d: %v", day, err)
				continue
			}
			listingData = append(listingData, batchData...)
		}

		// Process programs - now properly correlated with channels
		for channelIndex, channel := range filteredLineup {
			// Get the listings for this specific channel
			if channelIndex < len(listingData) {
				programList := listingData[channelIndex]
				for _, program := range programList {
					// Convert times to local timezone
					programStartTime := strings.ReplaceAll(program.StartTime, "Z", "")
					startTime, err := time.Parse("2006-01-02T15:04", programStartTime)
					if err != nil {
						log.Printf("Error parsing time: %v", err)
						continue
					}
					startTime = startTime.In(loc)
					endTime := startTime.Add(time.Duration(program.RunTime) * time.Minute)

					// Create program entry
					prog := types.Program{
						Channel:  channel.ChannelNumber,
						Title:    program.Title,
						SubTitle: program.Subtitle,
						Start:    startTime.Format("2006-01-02T15:04:05-07:00"),
						End:      endTime.Format("2006-01-02T15:04:05-07:00"),
						Duration: program.RunTime,
					}

					// Set category based on type
					switch program.Type {
					case "M":
						prog.Category = "movie"
					case "N":
						prog.Category = "news"
					case "S":
						prog.Category = "sports"
					}

					// Check flags
					for _, flag := range program.Flags {
						switch flag {
						case "EI":
							prog.Category = "kids"
						case "HD":
							prog.Video.Quality = "HDTV"
						case "Stereo":
							prog.Audio.Stereo = "stereo"
						case "New":
							prog.New = true
						}
					}

					newPrograms = append(newPrograms, prog)
				}
			}
		}

		// Mark this day as processed
		processedDays[dayKey] = true
		log.Printf("Processed day: %s", dayKey)
	}

	// Combine with existing programs
	var allPrograms []types.Program

	if len(existingGuide.Programs) > 0 {
		allPrograms = append(allPrograms, existingGuide.Programs...)
	}

	// Add new programs
	allPrograms = append(allPrograms, newPrograms...)

	// Sort and remove duplicates
	sort.SliceStable(allPrograms, func(i, j int) bool {
		if allPrograms[i].Start == allPrograms[j].Start {
			return allPrograms[i].Channel < allPrograms[j].Channel
		}
		return allPrograms[i].Start < allPrograms[j].Start
	})

	// Remove duplicates (based on start time and channel)
	seen := make(map[string]bool)
	var uniquePrograms []types.Program
	for _, prog := range allPrograms {
		key := fmt.Sprintf("%s-%s", prog.Start, prog.Channel)
		if !seen[key] {
			seen[key] = true
			uniquePrograms = append(uniquePrograms, prog)
		}
	}

	// Remove programs that have already ended
	currentTime := time.Now()
	var filteredPrograms []types.Program
	for _, prog := range uniquePrograms {
		// Parse the start time
		startTime, err := time.Parse("2006-01-02T15:04:05-07:00", prog.Start)
		if err != nil {
			log.Printf("Error parsing start time: %v", err)
			continue
		}
		// Calculate end time (start time + duration)
		endTime := startTime.Add(time.Duration(prog.Duration) * time.Minute)
		// Only keep programs that haven't ended yet
		if endTime.After(currentTime) {
			filteredPrograms = append(filteredPrograms, prog)
		}
	}

	// Create output structure with unique programs
	output := types.Guide{
		Channels:  filteredLineup,
		Programs:  filteredPrograms,
		Generated: time.Now().Format(time.RFC3339),
	}

	// Output JSON to file
	outputData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		log.Fatalf("Error encoding JSON: %v", err)
	}
	if err := os.WriteFile(config.GuideFile, outputData, 0644); err != nil {
		log.Fatalf("Error writing output file: %v", err)
	}

	// Save state
	stateData, err = json.Marshal(processedDays)
	if err != nil {
		log.Printf("Error saving state: %v", err)
	} else {
		os.WriteFile(config.StateFile, stateData, 0644) //nolint: errcheck
	}

	log.Printf("Successfully generated %s", config.GuideFile)
}
