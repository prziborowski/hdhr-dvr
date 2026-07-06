package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	pkgcfg "github.com/prziborowski/hdhr-dvr/pkg/config"
	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

const titanTVBaseURL = "https://titantv.com/api"

func fetchTitanTVChannels(userId, lineupId string) ([]types.TitanTVChannel, error) {
	url := fmt.Sprintf("%s/channel/%s/%s", titanTVBaseURL, userId, lineupId)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned non-OK status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response types.TitanTVLineupResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	return response.Channels, nil
}

func fetchTitanTVScheduleBlock(userId, lineupId string, startTime time.Time) (*types.TitanTVScheduleResponse, error) {
	dateStr := startTime.Format("200601021504")
	url := fmt.Sprintf("%s/schedule/%s/%s/%s/360", titanTVBaseURL, userId, lineupId, dateStr)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned non-OK status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response types.TitanTVScheduleResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	return &response, nil
}

func fetchLocalChannels() ([]types.Channel, error) {
	resp, err := http.Get("http://localhost:8080/api/channels")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local channels API returned non-OK status: %d", resp.StatusCode)
	}

	var channels []types.Channel
	if err := json.NewDecoder(resp.Body).Decode(&channels); err != nil {
		return nil, err
	}

	return channels, nil
}

func main() {
	// Load configuration
	config, err := pkgcfg.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	loc, err := time.LoadLocation(config.Timezone)
	if err != nil {
		log.Fatalf("Invalid timezone %s: %v", config.Timezone, err)
	}

	log.Printf("Fetching guide data from TitanTV for UserID: %s and LineupID: %s", config.UserID, config.LineUpID)

	// 1. Fetch Local Channels for filtering
	localChannels, err := fetchLocalChannels()
	if err != nil {
		log.Printf("Warning: Could not fetch local channels from API: %v. Guide will be empty or unfiltered.", err)
	}
	localChannelMap := make(map[string]bool)
	for _, ch := range localChannels {
		localChannelMap[ch.GuideNumber] = true
	}

	// 2. Fetch TitanTV Channels
	titanChannels, err := fetchTitanTVChannels(config.UserID, config.LineUpID)
	if err != nil {
		log.Fatalf("Error fetching TitanTV channels: %v", err)
	}
	log.Printf("Found %d titanTV channels", len(titanChannels))

	// Map channelIndex -> TitanTVChannel for easy lookup and prepare output LineupData
	channelMap := make(map[int]types.TitanTVChannel)
	var filteredLineup []types.LineupData

	for _, ch := range titanChannels {
		channelMap[ch.ChannelIndex] = ch

		channelNum := fmt.Sprintf("%d", ch.MajorChannel)
		if ch.MinorChannel != 0 {
			channelNum += fmt.Sprintf(".%d", ch.MinorChannel)
		}

		// Only include in lineup if it's a local channel
		if len(localChannelMap) > 0 && !localChannelMap[channelNum] {
			continue
		}

		filteredLineup = append(filteredLineup, types.LineupData{
			StationID:       fmt.Sprintf("%d", ch.ChannelID),
			ChannelNumber:   channelNum,
			StationCallSign: ch.CallSign,
			Logo:            ch.Logo,
		})
	}

	// 3. Fetch Schedule in blocks (matching titantv_grabber.py logic)
	var allPrograms []types.Program
	seenPrograms := make(map[string]bool)
	startTime := time.Now().In(loc).Truncate(time.Hour)

	log.Printf("Fetching schedule starting from %s", startTime.Format(time.RFC3339))

	for i := 0; i < 28; i++ { // 7 days * 4 blocks of 6 hours per day = 28 blocks
		blockStartTime := startTime.Add(time.Duration(i*6) * time.Hour)
		log.Printf("Fetching block %d/28 (starting %s)...", i+1, blockStartTime.Format("2006-01-02 15:04"))

		schedResp, err := fetchTitanTVScheduleBlock(config.UserID, config.LineUpID, blockStartTime)
		if err != nil {
			log.Printf("Error fetching schedule block %d: %v", i+1, err)
			continue
		}

		for _, chSched := range schedResp.Channels {
			chInfo, ok := channelMap[chSched.ChannelIndex]
			if !ok {
				log.Printf("Warning: No channel info found for index %d", chSched.ChannelIndex)
				continue
			}

			channelNum := fmt.Sprintf("%d", chInfo.MajorChannel)
			if chInfo.MinorChannel != 0 {
				channelNum += fmt.Sprintf(".%d", chInfo.MinorChannel)
			}

			// Filter out programs that are not in the local channel list
			if len(localChannelMap) > 0 && !localChannelMap[channelNum] {
				continue
			}

			for _, day := range chSched.Days {
				for _, evt := range day.Events {
					// Parse TitanTV ISO 8601 time: "2026-01-29T10:00:00"
					start, err := time.ParseInLocation("2006-01-02T15:04:05", evt.StartTime, loc)
					if err != nil {
						start, err = time.ParseInLocation("2006-01-02T15:04", evt.StartTime, loc)
						if err != nil {
							log.Printf("Error parsing start time %s: %v", evt.StartTime, err)
							continue
						}
					}

					end, err := time.ParseInLocation("2006-01-02T15:04:05", evt.EndTime, loc)
					if err != nil {
						end, err = time.ParseInLocation("2006-01-02T15:04", evt.EndTime, loc)
						if err != nil {
							log.Printf("Error parsing end time %s: %v", evt.EndTime, err)
							continue
						}
					}

					// De-duplication key: channel + start time
					progKey := fmt.Sprintf("%s|%s", channelNum, evt.StartTime)
					if seenPrograms[progKey] {
						continue
					}
					seenPrograms[progKey] = true

					// Map to types.Program
					prog := types.Program{
						Channel:  channelNum,
						Title:    evt.Title,
						SubTitle: evt.SubTitle,
						Start:    start.In(loc).Format("2006-01-02T15:04:05-07:00"),
						End:      end.In(loc).Format("2006-01-02T15:04:05-07:00"),
						Duration: int(end.Sub(start).Minutes()),
					}

					if evt.ProgramType == "Movie" {
						prog.Category = "movie"
					} else if evt.ProgramType == "Sports" {
						prog.Category = "sports"
					} else if evt.ProgramType == "News" {
						prog.Category = "news"
					}

					if evt.IsNew {
						prog.New = true
					}

					allPrograms = append(allPrograms, prog)
				}
			}
		}

		time.Sleep(5 * time.Second)
	}

	sort.SliceStable(allPrograms, func(i, j int) bool {
		if allPrograms[i].Start == allPrograms[j].Start {
			return allPrograms[i].Channel < allPrograms[j].Channel
		}
		return allPrograms[i].Start < allPrograms[j].Start
	})

	output := types.Guide{
		Channels:  filteredLineup,
		Programs:  allPrograms,
		Generated: time.Now().Format(time.RFC3339),
	}

	outputData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		log.Fatalf("Error encoding JSON: %v", err)
	}
	if err := os.WriteFile(config.GuideFile, outputData, 0644); err != nil {
		log.Fatalf("Error writing output file: %v", err)
	}

	log.Printf("Successfully generated %s with %d programs", config.GuideFile, len(allPrograms))
}
