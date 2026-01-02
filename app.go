package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
)

type Channel struct {
	GuideNumber    string `json:"GuideNumber"`
	GuideName      string `json:"GuideName"`
	VideoCodec     string `json:"VideoCodec"`
	AudioCodec     string `json:"AudioCodec"`
	HD             int    `json:"HD"`
	SignalStrength int    `json:"SignalStrength"`
	SignalQuality  int    `json:"SignalQuality"`
	URL            string `json:"URL"`
}

type Recording struct {
	ID        int
	ChannelID string
	Date      string // YYYY-MM-DD
	StartTime string // HH:MM
	Duration  int    // Duration in minutes
	Status    string
	CreatedAt time.Time
}

type RecordingRequest struct {
	ChannelID string `json:"channelId"`
	Date      string `json:"date"`      // YYYY-MM-DD
	StartTime string `json:"startTime"` // HH:MM
	Duration  int    `json:"duration"`  // Duration in minutes
}

var (
	db          *sql.DB
	recordingCh = make(chan Recording, 100)
)

func main() {
	// Initialize database
	var err error
	db, err = sql.Open("sqlite3", "./recordings.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close() //nolint: errcheck

	// Create tables if they don't exist
	createTables()

	// Load channels from HDHomeRun
	loadChannels()

	// Load existing recordings
	loadRecordings()

	// Start recording scheduler
	go startRecordingScheduler()

	// Set up routes
	r := mux.NewRouter()

	// Routes
	r.HandleFunc("/", serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/api/channels", getChannels).Methods("GET")
	r.HandleFunc("/api/recordings", getRecordings).Methods("GET")
	r.HandleFunc("/api/recordings", createRecording).Methods("POST")
	r.HandleFunc("/api/recordings/{id}", deleteRecording).Methods("DELETE")
	r.HandleFunc("/api/recordings/{id}/file", getRecordingFile).Methods("GET", "HEAD")

	// Start server
	log.Println("Server starting on :8080...")
	log.Fatal(http.ListenAndServe(":8080", r))
}

func createTables() {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS channels (
            guide_number TEXT PRIMARY KEY,
            guide_name TEXT,
            url TEXT
        );

        CREATE TABLE IF NOT EXISTS recordings (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            channel_id TEXT,
            date TEXT,
            start_time TEXT,
            duration INTEGER,  -- Changed from end_time to duration
            status TEXT DEFAULT 'pending',
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            FOREIGN KEY(channel_id) REFERENCES channels(guide_number)
        );
        CREATE INDEX IF NOT EXISTS idx_recordings_channel ON recordings(channel_id);
    `)
	if err != nil {
		log.Fatal(err)
	}
}

func loadChannels() {
	log.Println("Fetching channels")
	resp, err := http.Get("http://hdhomerun.local/lineup.json?show=found")
	if err != nil {
		log.Printf("Error fetching channels: %v", err)
		return
	}
	defer resp.Body.Close() //nolint: errcheck

	var chs []Channel
	if err := json.NewDecoder(resp.Body).Decode(&chs); err != nil {
		log.Printf("Error decoding channels: %v", err)
		return
	}

	// Store channels in database
	tx, _ := db.Begin()
	for _, ch := range chs {
		_, err := tx.Exec("INSERT OR IGNORE INTO channels (guide_number, guide_name, url) VALUES (?, ?, ?)",
			ch.GuideNumber, ch.GuideName, ch.URL)
		if err != nil {
			log.Printf("Error storing channel: %v", err)
		}
	}
	tx.Commit() //nolint: errcheck
}

func loadRecordings() {
	log.Println("Load recordings")
	// Get all pending recordings from database
	rows, err := db.Query(`
        SELECT id, channel_id, date, start_time, duration, status
        FROM recordings
        WHERE status = 'pending'
    `)
	if err != nil {
		log.Printf("Error loading recordings: %v", err)
		return
	}
	defer rows.Close() //nolint: errcheck

	// Get system timezone
	loc, err := getLocalLocation()
	if err != nil {
		loc = time.UTC
		log.Printf("Error loading system timezone, using UTC: %v", err)
	}

	// Process each recording
	for rows.Next() {
		var r Recording
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.Date, &r.StartTime, &r.Duration, &r.Status); err != nil {
			log.Printf("Error scanning recording: %v", err)
			continue
		}

		// Parse the start time in system timezone
		dateTimeStr := fmt.Sprintf("%s %s", r.Date, r.StartTime)
		startTime, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
		if err != nil {
			log.Printf("Error parsing start time for recording %d: %v", r.ID, err)
			continue
		}

		// Calculate end time
		endTime := startTime.Add(time.Duration(r.Duration) * time.Minute)

		// Check if recording should start now
		now := time.Now().In(loc)
		if now.After(startTime) && now.Before(endTime) {
			// Recording is already in progress
			log.Printf("Recording %d is already in progress", r.ID)
		} else if now.Before(startTime) {
			// Schedule recording for later
			recordingCh <- r
		} else {
			// Recording should have started already
			log.Printf("Recording %d should have started at %v", r.ID, startTime)
		}
	}
}

func startRecordingScheduler() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	loc, err := getLocalLocation()
	if err != nil {
		log.Printf("Error determining timezone: %v", err)
		loc = time.UTC
	}

	for {
		select {
		case <-ticker.C:
			now := time.Now().In(loc)

			// Load recordings from database
			rows, err := db.Query("SELECT id, channel_id, date, start_time, duration, status FROM recordings WHERE status = 'pending'")
			if err != nil {
				log.Printf("Error loading recordings: %v", err)
				continue
			}

			var recordings []Recording
			for rows.Next() {
				var r Recording
				if err := rows.Scan(&r.ID, &r.ChannelID, &r.Date, &r.StartTime, &r.Duration, &r.Status); err != nil {
					log.Printf("Error scanning recording: %v", err)
					continue
				}
				recordings = append(recordings, r)
			}
			rows.Close() //nolint: errcheck

			for _, r := range recordings {
				startTime, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%s %s", r.Date, r.StartTime), loc)
				if err != nil {
					log.Printf("Error parsing start time: %v", err)
					continue
				}

				if now.After(startTime) && now.Before(startTime.Add(1*time.Minute)) {
					// Start recording
					go startRecording(r)
				}
			}
		case <-recordingCh:
			// Recording was already stored in database in createRecording
			// No need to add to in-memory slice
		}
	}
}

func startRecording(r Recording) {
	// Find the channel
	var ch Channel
	err := db.QueryRow("SELECT guide_number, guide_name, url FROM channels WHERE guide_number = ?", r.ChannelID).Scan(
		&ch.GuideNumber, &ch.GuideName, &ch.URL)
	if err != nil {
		log.Printf("Error finding channel %s: %v", r.ChannelID, err)
		return
	}

	// Create output filename
	outputDir := os.Getenv("STORAGE_DIR")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Printf("Error creating output directory: %v", err)
		return
	}
	outputFile := filepath.Join(outputDir, fmt.Sprintf("%s-%s-%s-%s.mp4",
		r.Date, r.StartTime, ch.GuideName, ch.GuideNumber))

	// Create log file in /tmp
	logFile := filepath.Join("/tmp", fmt.Sprintf("ffmpeg-%s-%s.log", r.Date, r.StartTime))
	logFileHandle, err := os.Create(logFile)
	if err != nil {
		log.Printf("Error creating log file: %v", err)
		return
	}
	defer logFileHandle.Close() //nolint: errcheck

	// Build ffmpeg command with duration in seconds
	durationSeconds := r.Duration * 60
	cmd := exec.Command("ffmpeg",
		"-i", ch.URL,
		"-t", fmt.Sprintf("%d", durationSeconds),
		"-c", "copy",
		outputFile)

	// Set up logging
	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle

	// Detailed logging
	log.Printf("Starting recording: %s", outputFile)
	log.Printf("Channel: %s (%s)", ch.GuideName, ch.GuideNumber)
	log.Printf("Date: %s, Time: %s, Duration: %d minutes", r.Date, r.StartTime, r.Duration)
	log.Printf("Log file: %s", logFile)
	log.Printf("FFmpeg command: ffmpeg -i %s -t %d -c copy %s",
		ch.URL, durationSeconds, outputFile)

	// Start recording
	if err := cmd.Run(); err != nil {
		log.Printf("Error running ffmpeg: %v", err)
		return
	}

	// Update recording status
	_, err = db.Exec("UPDATE recordings SET status = 'completed' WHERE id = ?", r.ID)
	if err != nil {
		log.Printf("Error updating recording status: %v", err)
	}

	// Final log message
	log.Printf("Recording completed successfully: %s", outputFile)
}

// serveHome serves the main HTML page
func serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "templates/index.html")
}

// getChannels returns the list of available channels
func getChannels(w http.ResponseWriter, r *http.Request) {
	// Get channels from database
	rows, err := db.Query("SELECT guide_number, guide_name FROM channels")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close() //nolint: errcheck

	// Define the struct with proper JSON tags
	type channelResponse struct {
		GuideNumber string `json:"guideNumber"`
		GuideName   string `json:"guideName"`
	}

	var channelList []channelResponse

	for rows.Next() {
		var ch channelResponse
		if err := rows.Scan(&ch.GuideNumber, &ch.GuideName); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		channelList = append(channelList, ch)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(channelList) //nolint: errcheck
}

func getRecordings(w http.ResponseWriter, r *http.Request) {
	// Get recordings with channel information
	rows, err := db.Query(`
        SELECT r.id, r.channel_id, r.date, r.start_time, r.duration, r.status,
               c.guide_number, c.guide_name
        FROM recordings r
        LEFT JOIN channels c ON r.channel_id = c.guide_number
		  ORDER BY r.start_time
    `)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close() //nolint: errcheck

	var recordings []struct {
		ID          int    `json:"id"`
		ChannelID   string `json:"channel_id"`
		Date        string `json:"date"`
		StartTime   string `json:"start_time"`
		Duration    int    `json:"duration"`
		Status      string `json:"status"`
		GuideNumber string `json:"guide_number"`
		GuideName   string `json:"guide_name"`
	}

	for rows.Next() {
		var r struct {
			ID          int    `json:"id"`
			ChannelID   string `json:"channel_id"`
			Date        string `json:"date"`
			StartTime   string `json:"start_time"`
			Duration    int    `json:"duration"`
			Status      string `json:"status"`
			GuideNumber string `json:"guide_number"`
			GuideName   string `json:"guide_name"`
		}
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.Date, &r.StartTime, &r.Duration, &r.Status,
			&r.GuideNumber, &r.GuideName); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		recordings = append(recordings, r)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(recordings) //nolint: errcheck
}

func getRecordingFile(w http.ResponseWriter, r *http.Request) {
	// Debug: Log all request headers
	log.Printf("Request headers for recording file:")
	for name, values := range r.Header {
		for _, value := range values {
			log.Printf("  %s: %s", name, value)
		}
	}

	// Extract ID from URL
	idStr := mux.Vars(r)["id"]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid recording ID", http.StatusBadRequest)
		return
	}

	// Get recording from database
	var recording Recording
	var channelName string
	err = db.QueryRow(`
        SELECT r.id, r.channel_id, r.date, r.start_time, r.duration, r.status,
               c.guide_name
        FROM recordings r
        LEFT JOIN channels c ON r.channel_id = c.guide_number
        WHERE r.id = ?
    `, id).Scan(&recording.ID, &recording.ChannelID, &recording.Date,
		&recording.StartTime, &recording.Duration, &recording.Status, &channelName)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Recording not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Check if recording is completed
	if recording.Status != "completed" {
		http.Error(w, "Recording not completed", http.StatusForbidden)
		return
	}

	// Get storage directory from environment variable or use default
	storageDir := os.Getenv("STORAGE_DIR")

	// Construct the file path
	filePath := filepath.Join(storageDir, fmt.Sprintf("%s-%s-%s-%s.mp4",
		recording.Date, recording.StartTime, channelName, recording.ChannelID))

	// Check if file exists
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Recording file not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close() //nolint: errcheck

	// Handle Range header for partial content
	rangeHeader := r.Header.Get("Range")
	log.Printf("Range header received: %s", rangeHeader)

	if rangeHeader != "" {
		log.Printf("Processing Range request for bytes %s", rangeHeader)

		// Parse the range header
		rangeParts := strings.Split(rangeHeader, "=")
		if len(rangeParts) != 2 || rangeParts[0] != "bytes" {
			log.Printf("Invalid range header format: %s", rangeHeader)
			http.Error(w, "Invalid range header", http.StatusBadRequest)
			return
		}

		rangeValues := strings.Split(rangeParts[1], "-")
		if len(rangeValues) < 1 || len(rangeValues) > 2 {
			log.Printf("Invalid range values: %s", rangeParts[1])
			http.Error(w, "Invalid range header", http.StatusBadRequest)
			return
		}

		// Handle cases where end is omitted (e.g., "bytes=0-")
		start, err1 := strconv.ParseInt(rangeValues[0], 10, 64)
		if err1 != nil {
			log.Printf("Error parsing start value: %v", err1)
			http.Error(w, "Invalid range header", http.StatusBadRequest)
			return
		}

		var end int64
		if len(rangeValues) == 2 && rangeValues[1] != "" {
			end, err1 = strconv.ParseInt(rangeValues[1], 10, 64)
			if err1 != nil {
				log.Printf("Error parsing end value: %v", err1)
				http.Error(w, "Invalid range header", http.StatusBadRequest)
				return
			}
		} else {
			// End is omitted, use file size
			end = fileInfo.Size() - 1
		}

		// Validate range
		fileSize := fileInfo.Size()
		if start < 0 || end >= fileSize || start > end {
			log.Printf("Invalid range: start=%d, end=%d, fileSize=%d", start, end, fileSize)
			http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		// Set headers for partial content
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)

		log.Printf("Sending partial content: bytes %d-%d", start, end)

		// Seek to the start position
		_, err = file.Seek(start, io.SeekStart)
		if err != nil {
			log.Printf("Error seeking to position %d: %v", start, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Limit the reader to the requested range
		limitedReader := io.LimitReader(file, end-start+1)

		// Stream the partial content
		_, err = io.Copy(w, limitedReader)
		if err != nil {
			log.Printf("Error serving partial content: %v", err)
		}
		return
	} else {
		log.Printf("No Range header received - sending full file")
	}

	// For full file requests
	fileSize := fileInfo.Size()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filepath.Base(filePath)))
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
	w.Header().Set("Accept-Ranges", "bytes")

	log.Printf("Sending full file of size %d bytes", fileSize)

	// Stream the file to the client
	_, err = io.Copy(w, file)
	if err != nil {
		log.Printf("Error serving file: %v", err)
	}
}

func createRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RecordingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate duration
	if req.Duration <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{ //nolint: errcheck
			"error": "Duration must be positive",
		})
		return
	}

	// Verify channel exists
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM channels WHERE guide_number = ?)", req.ChannelID).Scan(&exists)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{ //nolint: errcheck
			"error": "Channel not found",
		})
		return
	}

	// Create new recording
	recording := Recording{
		ChannelID: req.ChannelID,
		Date:      req.Date,
		StartTime: req.StartTime,
		Duration:  req.Duration,
		Status:    "pending",
	}

	// Store in database
	result, err := db.Exec(`
        INSERT INTO recordings (channel_id, date, start_time, duration, status)
        VALUES (?, ?, ?, ?, ?)
    `, recording.ChannelID, recording.Date, recording.StartTime, recording.Duration, recording.Status)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{ //nolint: errcheck
			"error": "Failed to create recording",
		})
		return
	}

	// Get the ID of the newly inserted recording
	id, err := result.LastInsertId()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{ //nolint: errcheck
			"error": "Failed to get recording ID",
		})
		return
	}

	recording.ID = int(id)

	// Send to recording channel
	recordingCh <- recording

	// Return the created recording
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(recording) //nolint: errcheck
}

func deleteRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from URL
	idStr := r.URL.Path[len("/api/recordings/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid recording ID", http.StatusBadRequest)
		return
	}

	// Delete from database
	_, err = db.Exec("DELETE FROM recordings WHERE id = ?", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func getLocalLocation() (*time.Location, error) {
	// Try to get system timezone
	tz, err := time.LoadLocation("America/Los_Angeles")
	log.Printf("Using timezone %s\n", tz.String())
	if err != nil {
		// Fallback to UTC if system timezone can't be determined
		return time.UTC, nil
	}
	return tz, nil
}
