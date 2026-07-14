package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"

	pkgcfg "github.com/prziborowski/hdhr-dvr/pkg/config"
	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

type RecordingRequest struct {
	ChannelID string  `json:"channelId"`
	Date      string  `json:"date"`      // YYYY-MM-DD
	StartTime string  `json:"startTime"` // HH:MM
	Duration  int     `json:"duration"`  // Duration in minutes
	Title     *string `json:"title,omitempty"`
}

const (
	PreRollSeconds  = 30
	PostRollMinutes = 1
	queryTimeout    = 10 * time.Second
)

var (
	db             *sql.DB
	recordingCh    = make(chan types.Recording, 100)
	config         *pkgcfg.Config
	tunerCount     int
	guideData      map[string]interface{}
	guideDataMutex sync.RWMutex
	watcher        *fsnotify.Watcher
	// Track running ffmpeg processes
	runningProcesses   sync.Map // key: recording ID, value: *exec.Cmd
	shutdownInProgress bool
	shutdownMutex      sync.Mutex
)

func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "./recordings.db")
	if err != nil {
		return nil, err
	}

	// Set connection pool parameters
	db.SetMaxOpenConns(10)   // Maximum number of open connections
	db.SetMaxIdleConns(5)    // Maximum number of idle connections
	db.SetConnMaxLifetime(0) // No connection lifetime limit

	// Test the connection
	err = db.Ping()
	if err != nil {
		return nil, err
	}

	return db, nil
}

func main() {
	// Initialize database
	var err error
	db, err = initDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close() //nolint: errcheck

	config, err = pkgcfg.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	tunerCount = fetchTunerCount()
	log.Printf("System initialized with %d tuners", tunerCount)

	// Create tables if they don't exist
	createTables()

	// Load channels from HDHomeRun
	loadChannels()

	// Load guide data
	if loadGuide() {
		go setupFileWatcher(config.GuideFile)
	}

	// Load existing recordings
	loadRecordings()

	// Clean up old recordings
	cleanupOldRecordings()

	// Start recording scheduler
	go startRecordingScheduler()

	// Start periodic cleanup
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			cleanupOldRecordings()
		}
	}()

	// Set up routes
	r := mux.NewRouter()

	// Page routes (All serve the same index.html for SPA routing)
	r.HandleFunc("/", serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/schedule", serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/recordings", serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/guide", serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/keywords", serveHome).Methods("GET", "HEAD")

	// API Routes
	r.HandleFunc("/api/channels", getChannels).Methods("GET")
	r.HandleFunc("/api/recordings", getRecordings).Methods("GET")
	r.HandleFunc("/api/recordings", createRecording).Methods("POST")
	r.HandleFunc("/api/recordings/{id}", deleteRecording).Methods("DELETE")
	r.HandleFunc("/api/recordings/{id}", updateRecording).Methods("PATCH")
	r.HandleFunc("/api/recordings/{id}/file", getRecordingFile).Methods("GET", "HEAD")
	r.HandleFunc("/api/guide", getGuide).Methods("GET")
	// Auto-record keywords routes
	r.HandleFunc("/api/keywords", getKeywords).Methods("GET")
	r.HandleFunc("/api/keywords", createKeyword).Methods("POST")
	r.HandleFunc("/api/keywords/{id}", deleteKeyword).Methods("DELETE")

	// Start server
	log.Println("Server starting on :8080...")
	server := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	// Handle graceful shutdown
	go func() {
		// Wait for interrupt signal
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		shutdownMutex.Lock()
		if shutdownInProgress {
			shutdownMutex.Unlock()
			return
		}
		shutdownInProgress = true
		shutdownMutex.Unlock()

		log.Println("\nReceived shutdown signal...")

		// Check for active recordings
		activeCount := 0
		runningProcesses.Range(func(key, value interface{}) bool {
			activeCount++
			return true
		})

		if activeCount > 0 {
			log.Printf("WARNING: %d recording(s) in progress. Terminating...", activeCount)

			// Terminate all running ffmpeg processes
			runningProcesses.Range(func(key, value interface{}) bool {
				if cmd, ok := value.(*exec.Cmd); ok && cmd.Process != nil {
					log.Printf("Terminating recording %d...", key)
					if err := cmd.Process.Kill(); err != nil {
						log.Printf("Error killing recording %d: %v", key, err)
					}
				}
				return true
			})

			// Wait a moment for processes to terminate
			time.Sleep(2 * time.Second)

			log.Println("Recordings terminated. Proceeding with shutdown...")
		}

		log.Println("No active recordings. Shutting down gracefully...")
		time.Sleep(1 * time.Second)

		// Shutdown the server
		if err := server.Shutdown(context.Background()); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	log.Fatal(server.ListenAndServe())
}

func updateRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PATCH" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusBadRequest)
		return
	}

	idStr := mux.Vars(r)["id"]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid recording ID", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	var updateReq struct {
		Title *string `json:"title"`
	}

	if err := json.NewDecoder(r.Body).Decode(&updateReq); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if updateReq.Title == nil {
		http.Error(w, "Title is required", http.StatusBadRequest)
		return
	}

	result, err := dbExecContext(ctx, "UPDATE recordings SET title = ? WHERE id = ? AND status = 'pending'", *updateReq.Title, id)
	if err != nil {
		log.Printf("Error updating recording: %v", err)
		http.Error(w, "Failed to update recording", http.StatusInternalServerError)
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Error getting rows affected: %v", err)
	}
	if rowsAffected == 0 {
		http.Error(w, "Recording not found or not pending", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func createTables() {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS channels (
            guide_number TEXT PRIMARY KEY,
            guide_name TEXT,
            url TEXT,
            enabled INTEGER DEFAULT 1
        );

        CREATE TABLE IF NOT EXISTS recordings (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            channel_id TEXT,
            date TEXT,
            start_time TEXT,
            title TEXT,
            duration INTEGER,
            status TEXT DEFAULT 'pending',
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            file_size INTEGER DEFAULT 0,
            FOREIGN KEY(channel_id) REFERENCES channels(guide_number)
        );
        CREATE INDEX IF NOT EXISTS idx_recordings_channel ON recordings(channel_id);

        CREATE TABLE IF NOT EXISTS keywords (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            name TEXT UNIQUE NOT NULL,
            category TEXT DEFAULT '',
            enabled INTEGER DEFAULT 1,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP
        );
    `)
	if err != nil {
		log.Fatal(err)
	}
}

func loadChannels() {
	log.Println("Fetching channels")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://hdhomerun.local/lineup.json?show=found", nil)
	if err != nil {
		log.Printf("Error creating channels request: %v", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Error fetching channels: %v", err)
		return
	}
	defer resp.Body.Close() //nolint: errcheck

	var chs []types.Channel
	if err := json.NewDecoder(resp.Body).Decode(&chs); err != nil {
		log.Printf("Error decoding channels: %v", err)
		return
	}

	// Store channels in database
	tx, err := db.Begin()
	if err != nil {
		log.Printf("Error starting transaction for channels: %v", err)
		return
	}

	// Clear current channels to ensure we only have the latest list
	_, err = tx.Exec("UPDATE channels SET enabled=0")
	if err != nil {
		log.Printf("Error clearing channels table: %v", err)
		tx.Rollback() //nolint: errcheck
		return
	}

	failedCount := 0
	for _, ch := range chs {
		_, err := tx.Exec("INSERT OR REPLACE INTO channels (guide_number, guide_name, url, enabled) VALUES (?, ?, ?, ?)",
			ch.GuideNumber, ch.GuideName, ch.URL, ch.Enabled == nil || *ch.Enabled == 1)
		if err != nil {
			log.Printf("Error storing channel %s: %v", ch.GuideNumber, err)
			failedCount++
		}
	}

	if failedCount > 0 {
		log.Printf("WARNING: %d channels failed to insert, rolling back transaction", failedCount)
		tx.Rollback() //nolint: errcheck
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing channels transaction: %v", err)
		tx.Rollback() //nolint: errcheck
	}
}

func loadRecordings() {
	log.Println("Load recordings")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting database transaction: %v", err)
		return
	}
	defer tx.Rollback() //nolint: errcheck

	rows, err := tx.QueryContext(ctx, `
        SELECT id, channel_id, date, start_time, duration, status, title, file_size
        FROM recordings
    `)
	if err != nil {
		log.Printf("Error loading recordings: %v", err)
		return
	}

	loc, _ := getLocalLocation()
	now := time.Now().In(loc)

	// Process each recording
	for rows.Next() {
		var r types.Recording
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.Date, &r.StartTime, &r.Duration, &r.Status, &r.Title, &r.FileSize); err != nil {
			log.Printf("Error scanning recording: %v", err)
			continue
		}

		dateTimeStr := fmt.Sprintf("%s %s", r.Date, r.StartTime)
		startTime, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
		if err != nil {
			log.Printf("Error parsing start time for recording %d: %v", r.ID, err)
			continue
		}

		adjustedStartTime := startTime.Add(-PreRollSeconds * time.Second)
		endTime := adjustedStartTime.Add(time.Duration(r.Duration+PostRollMinutes) * time.Minute)

		if now.After(endTime) {
			log.Printf("Skipping recording %d - already ended at %v", r.ID, endTime)
			continue
		}

		newStatus := r.CheckStatus(db, loc, config.StorageDir)
		if r.Status != newStatus {
			_, err := tx.ExecContext(ctx, "UPDATE recordings SET status = ? WHERE id = ?", newStatus, r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			} else {
				log.Printf("Updated recording %d status from %s to %s", r.ID, r.Status, newStatus)
				r.Status = newStatus
			}
		}

		if now.After(adjustedStartTime) && now.Before(endTime) {
			log.Printf("Recording %d is already in progress", r.ID)
		} else if now.Before(adjustedStartTime) {
			recordingCh <- r
		} else {
			log.Printf("Recording %d should have started at %v", r.ID, adjustedStartTime)
			go startRecording(r)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating recordings: %v", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing transaction: %v", err)
		tx.Rollback() //nolint: errcheck
		return
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
			rows, err := db.QueryContext(context.Background(), `
                SELECT id, channel_id, date, start_time, duration, status, title
                FROM recordings
    			WHERE status = 'pending'
    		`)
			if err != nil {
				log.Printf("Error loading recordings: %v", err)
				continue
			}
			var recordings []types.Recording
			for rows.Next() {
				var r types.Recording
				if err := rows.Scan(&r.ID, &r.ChannelID, &r.Date, &r.StartTime, &r.Duration, &r.Status, &r.Title); err != nil {
					log.Printf("Error scanning recording: %v", err)
					continue
				}
				recordings = append(recordings, r)
			}
			if err := rows.Err(); err != nil {
				log.Printf("Error iterating recordings: %v", err)
			}
			rows.Close() //nolint: errcheck

			// Start a precise timer for each recording
			for _, r := range recordings {
				// Check if this recording has already been scheduled
				if _, exists := recordingTimers.Load(r.ID); exists {
					continue
				}

				startTime, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%s %s", r.Date, r.StartTime), loc)
				if err != nil {
					log.Printf("Error parsing start time for recording %d: %v", r.ID, err)
					continue
				}

				// Calculate the actual start time (30 seconds before scheduled time)
				actualStartTime := startTime.Add(-PreRollSeconds * time.Second)

				// Only schedule if the recording hasn't already started
				if now.Before(actualStartTime) {
					// Start a precise timer for this recording
					go startRecordingTimer(r, actualStartTime)
				} else if now.Before(startTime.Add(time.Duration(r.Duration+PostRollMinutes) * time.Minute)) {
					// Recording should have started already, start it immediately
					log.Printf("Recording %d should have started at %v, starting now", r.ID, actualStartTime)
					go startRecording(r)
				} else {
					// Recording time has passed, mark as failed
					log.Printf("Recording %d should have started at %v but it's now %v, marking as failed",
						r.ID, actualStartTime, now)
					_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
					if err != nil {
						log.Printf("Error updating recording status: %v", err)
					}
				}
			}
		case <-recordingCh:
			// Recording was already stored in database in createRecording
			// No need to add to in-memory slice
		}
	}
}

// Track active recording timers
var recordingTimers sync.Map // key: recording ID, value: struct{}

func dbQueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return db.QueryContext(ctx, query, args...)
}

func dbExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return db.ExecContext(ctx, query, args...)
}

func dbQueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return db.QueryRowContext(ctx, query, args...)
}

// startRecordingTimer starts a precise timer for a recording
func startRecordingTimer(r types.Recording, startTime time.Time) {
	// Mark this recording as having a timer
	recordingTimers.Store(r.ID, struct{}{})
	defer recordingTimers.Delete(r.ID)

	// Calculate time until recording should start
	duration := time.Until(startTime)

	// If the recording should start very soon, just wait the remaining time
	if duration > 0 {
		time.Sleep(duration)
	}

	// Check if the recording still exists and hasn't been deleted
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var exists bool
	err := dbQueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM recordings WHERE id = ? AND status = 'pending')", r.ID).Scan(&exists)
	cancel()
	if err != nil {
		log.Printf("Error checking if recording %d exists: %v", r.ID, err)
		return
	}

	if !exists {
		log.Printf("Recording %d was deleted before start time, not starting", r.ID)
		return
	}

	// Start the recording
	log.Printf("Starting recording %d at %v (scheduled for %v)",
		r.ID, time.Now(), startTime.Add(PreRollSeconds*time.Second))
	go startRecording(r)
}

func startRecording(r types.Recording) {
	// First check: validate channel exists and get its info
	var ch types.Channel
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := dbQueryRowContext(ctx, "SELECT guide_number, guide_name, url FROM channels WHERE guide_number = ?", r.ChannelID).Scan(
		&ch.GuideNumber, &ch.GuideName, &ch.URL)
	cancel()
	if err != nil {
		log.Printf("Error finding channel %s: %v", r.ChannelID, err)
		// Mark as failed in a separate transaction
		_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
		if err != nil {
			log.Printf("Error updating recording status: %v", err)
		}
		return
	}

	// Get local timezone
	loc, err := getLocalLocation()
	if err != nil {
		log.Printf("Error determining timezone: %v", err)
		loc = time.UTC
	}

	// Parse the original start time
	dateTimeStr := fmt.Sprintf("%s %s", r.Date, r.StartTime)
	startTime, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
	if err != nil {
		log.Printf("Error parsing start time: %v", err)
		_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
		if err != nil {
			log.Printf("Error updating recording status: %v", err)
		}
		return
	}

	adjustedStartTime := startTime.Add(-PreRollSeconds * time.Second)
	adjustedDuration := r.Duration + PostRollMinutes

	log.Printf("Original start time: %v, Adjusted start time: %v, Original duration: %d, Adjusted duration: %d",
		startTime, adjustedStartTime, r.Duration, adjustedDuration)

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(config.StorageDir, 0755); err != nil {
		log.Printf("Error creating output directory: %v", err)
		// Mark as failed in a separate transaction
		_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
		if err != nil {
			log.Printf("Error updating recording status: %v", err)
		}
		return
	}

	// Use the original start time for the filename to maintain consistency with schedule
	outputFile := filepath.Join(config.StorageDir, r.GetFilePath())
	// Create log file in /tmp using original time
	logFile := filepath.Join("/tmp", fmt.Sprintf("ffmpeg-%s-%s.log", r.Date, r.StartTime))
	logFileHandle, err := os.Create(logFile)
	if err != nil {
		log.Printf("Error creating log file: %v", err)
		// Mark as failed in a separate transaction
		_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
		if err != nil {
			log.Printf("Error updating recording status: %v", err)
		}
		return
	}
	defer logFileHandle.Close() //nolint: errcheck

	// Build ffmpeg command with adjusted duration
	durationSeconds := adjustedDuration * 60
	ffmpegArgs := buildFFmpegArgs(ch.URL, durationSeconds, outputFile)
	cmd := exec.Command("ffmpeg", ffmpegArgs...)

	// Log the command
	log.Printf("Starting recording: %s", outputFile)
	log.Printf("Channel: %s (%s)", ch.GuideName, ch.GuideNumber)
	log.Printf("Original Date: %s, Original Time: %s, Adjusted Time: %v, Duration: %d minutes (original: %d)",
		r.Date, r.StartTime, adjustedStartTime.Format("15:04"), adjustedDuration, r.Duration)
	log.Printf("Storage directory: %s", config.StorageDir)
	log.Printf("Log file: %s", logFile)
	log.Printf("FFmpeg command: %s", getFFmpegCommandString(ch.URL, durationSeconds, outputFile))

	// Set up logging
	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle

	// Update status to "recording" with retry logic
	retryCount := 0
	maxRetries := 3
	for retryCount < maxRetries {
		txCtx, txCancel := context.WithTimeout(context.Background(), 5*time.Second)
		tx, err := db.BeginTx(txCtx, nil)
		txCancel()
		if err != nil {
			log.Printf("Error starting database transaction: %v", err)
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
			return
		}

		_, err = tx.ExecContext(context.Background(), "UPDATE recordings SET status = 'recording' WHERE id = ?", r.ID)
		if err != nil {
			log.Printf("Error updating recording status to 'recording': %v", err)
			tx.Rollback() //nolint: errcheck
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			tx.Rollback() //nolint: errcheck
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
			return
		}
		break
	}

	// Track the process
	runningProcesses.Store(r.ID, cmd)
	defer runningProcesses.Delete(r.ID)
	// Start recording (no database transaction held during this operation)

	var runErr error
	retryCount = 0
	maxRetries = 3
	backoff := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

	for retryCount <= maxRetries {
		runErr = cmd.Run()
		if runErr == nil {
			break
		}

		log.Printf("Error running ffmpeg (attempt %d/%d): %v", retryCount+1, maxRetries+1, runErr)

		if isHttpServerError(logFile) && retryCount < maxRetries {
			wait := backoff[retryCount]
			log.Printf("Detected HTTP server error, retrying in %v...", wait)
			time.Sleep(wait)

			// Re-create the command because exec.Cmd cannot be reused
			ffmpegArgs := buildFFmpegArgs(ch.URL, durationSeconds, outputFile)
			cmd = exec.Command("ffmpeg", ffmpegArgs...)
			cmd.Stdout = logFileHandle
			cmd.Stderr = logFileHandle
			runningProcesses.Store(r.ID, cmd)

			retryCount++
			continue
		}
		break
	}

	if runErr != nil {
		log.Printf("Error running ffmpeg after retries: %v", runErr)
		// Check if file was created despite the error
		if _, err := os.Stat(outputFile); err == nil {
			// File exists, mark as completed
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'completed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
		} else {
			// File doesn't exist, mark as failed
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
		}
		return
	}

	// Update status to "completed" with retry logic
	retryCount = 0
	for retryCount < maxRetries {
		txCtx, txCancel := context.WithTimeout(context.Background(), 5*time.Second)
		tx, err := db.BeginTx(txCtx, nil)
		txCancel()
		if err != nil {
			log.Printf("Error starting database transaction: %v", err)
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
			return
		}

		_, err = tx.ExecContext(context.Background(), "UPDATE recordings SET status = 'completed' WHERE id = ?", r.ID)
		if err != nil {
			log.Printf("Error updating recording status to 'completed': %v", err)
			tx.Rollback() //nolint: errcheck
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			tx.Rollback() //nolint: errcheck
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
			if err != nil {
				log.Printf("Error updating recording status: %v", err)
			}
			return
		}
		break
	}

	// Perform fast conversion from TS to MP4
	mp4File := strings.TrimSuffix(outputFile, filepath.Ext(outputFile)) + ".mp4"
	if err := convertToMp4(outputFile, mp4File); err != nil {
		log.Printf("Conversion warning: %v", err)
		// We don't mark as failed because the TS file still exists and is playable
	} else {
		// Optional: remove original TS file after successful conversion
		_ = os.Remove(outputFile)

		// Get final file size and update database
		if info, err := os.Stat(mp4File); err == nil {
			size := info.Size()
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET file_size = ? WHERE id = ?", size, r.ID)
			if err != nil {
				log.Printf("Error updating recording file size: %v", err)
			}
		} else {
			log.Printf("Error getting final recording file size: %v", err)
		}
	}

	// Final log message
	log.Printf("Recording completed successfully and converted to MP4: %s", mp4File)
}

// convertToMp4 performs a fast re-mux from TS to MP4 without re-encoding
func convertToMp4(tsFile, mp4File string) error {
	log.Printf("Converting %s to %s...", tsFile, mp4File)
	args := []string{
		"-i", tsFile,
		"-c", "copy", // Copy streams without re-encoding
		"-movflags", "+faststart", // Enable fast start for web streaming
		"-y", // Overwrite output file if it exists
		mp4File,
	}
	cmd := exec.Command("ffmpeg", args...)
	if err := cmd.Run(); err != nil {
		log.Printf("ffmpeg conversion failed: %v, attempting slower conversion", err)

		args = []string{
			"-err_detect", "ignore_err",
			"-fflags", "+genpts+discardcorrupt",
			"-i", tsFile,
			"-c", "copy", // Copy streams without re-encoding
			"-map", "0",
			"-f", "matroska",
			"-y", // Overwrite output file if it exists
			mp4File,
		}
		cmd = exec.Command("ffmpeg", args...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg conversion failed: %w", err)
		}
	}
	return nil
}

// buildFFmpegArgs builds the ffmpeg command arguments
func buildFFmpegArgs(inputURL string, durationSeconds int, outputFile string) []string {
	args := []string{
		// Input options
		"-i", inputURL,
		"-fflags", "+genpts", // Generate missing pts values
		"-analyzeduration", "100M", // Increase analysis duration
		"-probesize", "100M", // Increase probe size

		// Error handling options
		"-ignore_io_errors", "1", // Ignore I/O errors
		"-err_detect", "ignore_err", // Ignore errors
		"-max_interleave_delta", "100M", // Maximum interleave delta

		// Network options
		"-rtsp_transport", "tcp", // Use TCP for more reliable transport
		"-reconnect", "1", // Enable reconnection
		"-reconnect_at_eof", "1", // Reconnect when stream ends
		"-reconnect_streamed", "1", // Reconnect for streamed content
		"-reconnect_delay_max", "600", // Maximum reconnection delay in seconds

		// Output options
		"-t", fmt.Sprintf("%d", durationSeconds),
		"-c", "copy", // Stream copy mode
		"-f", "mpegts", // Force MPEG-TS format for reliability
		outputFile,
	}
	return args
}

// getFFmpegCommandString returns a human-readable string of the ffmpeg command
func getFFmpegCommandString(inputURL string, durationSeconds int, outputFile string) string {
	args := buildFFmpegArgs(inputURL, durationSeconds, outputFile)
	cmd := "ffmpeg " + strings.Join(args, " ")
	return cmd
}

// serveHome serves the main HTML page
func serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "templates/index.html")
}

// getChannels returns the list of available channels
func getChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := dbQueryContext(ctx, "SELECT guide_number, guide_name FROM channels WHERE enabled=1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

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
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating channels: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(channelList); err != nil {
		log.Printf("Error encoding channels response: %v", err)
	}
}

type GetRecordingsRec struct {
	ID          int     `json:"id"`
	ChannelID   string  `json:"channel_id"`
	Date        string  `json:"date"`
	StartTime   string  `json:"start_time"`
	Duration    int     `json:"duration"`
	Status      string  `json:"status"`
	Title       *string `json:"title,omitempty"`
	FileSize    int     `json:"file_size"`
	GuideNumber string  `json:"guide_number"`
	GuideName   string  `json:"guide_name"`
}

func getRecordings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := dbQueryContext(ctx, `
        SELECT r.id, r.channel_id, r.date, r.start_time, r.duration, r.status, r.title, r.file_size,
               c.guide_number, c.guide_name
        FROM recordings r
        LEFT JOIN channels c ON r.channel_id = c.guide_number
	  ORDER BY r.date, r.start_time
    `)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var recordings []GetRecordingsRec

	for rows.Next() {
		var r GetRecordingsRec
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.Date, &r.StartTime, &r.Duration, &r.Status,
			&r.Title, &r.FileSize, &r.GuideNumber, &r.GuideName); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		recordings = append(recordings, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating recordings: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(recordings); err != nil {
		log.Printf("Error encoding recordings response: %v", err)
	}
}

func getRecordingFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	var recording types.Recording
	var channelName string
	err = dbQueryRowContext(ctx, `
        SELECT r.id, r.channel_id, r.date, r.start_time, r.duration, r.status, r.title,
               c.guide_name
        FROM recordings r
        LEFT JOIN channels c ON r.channel_id = c.guide_number
        WHERE r.id = ?
    `, id).Scan(&recording.ID, &recording.ChannelID, &recording.Date,
		&recording.StartTime, &recording.Duration, &recording.Status, &recording.Title, &channelName)

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

	// Construct the file path
	filePath := filepath.Join(config.StorageDir, recording.GetFilePath())

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

	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

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
	err := dbQueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM channels WHERE guide_number = ?)", req.ChannelID).Scan(&exists)
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

	// Check for duplicate recording (channel + date + start time)
	var duplicateExists bool
	err = dbQueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM recordings WHERE channel_id = ? AND date = ? AND start_time = ?)", req.ChannelID, req.Date, req.StartTime).Scan(&duplicateExists)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if duplicateExists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{ //nolint: errcheck
			"error": "Recording already exists for this channel and time",
		})
		return
	}

	// Check tuner availability (max 4 concurrent recordings)
	startStr := req.Date + " " + req.StartTime
	reqStart, err := time.Parse("2006-01-02 15:04", startStr)
	if err != nil {
		http.Error(w, "Invalid date/time format", http.StatusBadRequest)
		return
	}
	reqEnd := reqStart.Add(time.Duration(req.Duration) * time.Minute)

	// Query recordings that overlap with the requested interval
	rows, err := dbQueryContext(ctx, `
		SELECT date, start_time, duration
		FROM recordings
		WHERE datetime(date || ' ' || start_time) < datetime(?, '+' || ? || ' minutes')
		  AND datetime(date || ' ' || start_time, '+' || duration || ' minutes') > datetime(?)`,
		startStr, req.Duration, startStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type event struct {
		t    time.Time
		diff int
	}
	var events []event
	for rows.Next() {
		var d, s string
		var dur int
		if err := rows.Scan(&d, &s, &dur); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		st, _ := time.Parse("2006-01-02 15:04", d+" "+s)
		et := st.Add(time.Duration(dur) * time.Minute)
		events = append(events, event{st, 1})
		events = append(events, event{et, -1})
	}

	sort.Slice(events, func(i, j int) bool {
		if events[i].t.Equal(events[j].t) {
			return events[i].diff < events[j].diff // End before Start at same time
		}
		return events[i].t.Before(events[j].t)
	})

	currentTuners := 0
	for _, e := range events {
		currentTuners += e.diff
		// Check if the current count exceeds capacity within the requested window
		if (e.t.After(reqStart) || e.t.Equal(reqStart)) && (e.t.Before(reqEnd) || e.t.Equal(reqEnd)) {
			if currentTuners >= tunerCount-1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "No tuners available during this time period"})
				return
			}
		}
	}

	// Use a transaction for the database operations
	txCtx, txCancel := context.WithTimeout(ctx, 5*time.Second)
	defer txCancel()
	tx, err := db.BeginTx(txCtx, nil)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint: errcheck

	// Create new recording
	recording := types.Recording{
		ChannelID: req.ChannelID,
		Date:      req.Date,
		StartTime: req.StartTime,
		Duration:  req.Duration,
		Status:    "pending",
		Title:     req.Title,
	}

	// Store in database
	result, err := tx.ExecContext(ctx, `
        INSERT INTO recordings (channel_id, date, start_time, duration, status, title)
        VALUES (?, ?, ?, ?, ?, ?)
    `, recording.ChannelID, recording.Date, recording.StartTime, recording.Duration, recording.Status, recording.Title)
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

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		tx.Rollback() //nolint: errcheck
		return
	}

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

	ctx := r.Context()
	// Extract ID from URL
	idStr := mux.Vars(r)["id"]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid recording ID", http.StatusBadRequest)
		return
	}

	// Delete from database
	_, err = dbExecContext(ctx, "DELETE FROM recordings WHERE id = ?", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Clean up any active timer for this recording
	recordingTimers.Delete(id)

	w.WriteHeader(http.StatusNoContent)
}

func getKeywords(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := dbQueryContext(ctx, "SELECT id, name, category, enabled, created_at FROM keywords ORDER BY created_at DESC")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var keywords []types.Keyword
	for rows.Next() {
		var k types.Keyword
		var category string
		var enabled int
		if err := rows.Scan(&k.ID, &k.Name, &category, &enabled, &k.CreatedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		k.Category = category
		k.Enabled = enabled == 1
		keywords = append(keywords, k)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating keywords: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(keywords); err != nil {
		log.Printf("Error encoding keywords response: %v", err)
	}
}

func createKeyword(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusBadRequest)
		return
	}

	var req struct {
		Name     string `json:"name"`
		Category string `json:"category,omitempty"`
		Enabled  *bool  `json:"enabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"}) //nolint: errcheck
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Keyword name cannot be empty"}) //nolint: errcheck
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	result, err := db.Exec("INSERT INTO keywords (name, category, enabled) VALUES (?, ?, ?)", req.Name, req.Category, enabled)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "Keyword already exists"}) //nolint: errcheck
			return
		}
		log.Printf("Error creating keyword: %v", err)
		http.Error(w, "Failed to create keyword", http.StatusInternalServerError)
		return
	}

	id, _ := result.LastInsertId()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "name": req.Name, "category": req.Category}) //nolint: errcheck
}

func deleteKeyword(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := mux.Vars(r)["id"]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid keyword ID", http.StatusBadRequest)
		return
	}

	result, err := db.Exec("DELETE FROM keywords WHERE id = ?", id)
	if err != nil {
		log.Printf("Error deleting keyword: %v", err)
		http.Error(w, "Failed to delete keyword", http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Keyword not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func getLocalLocation() (*time.Location, error) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.UTC, nil
	}
	return loc, nil
}

// Load the guide data from file
func loadGuide() bool {
	if _, err := os.Stat(config.GuideFile); os.IsNotExist(err) {
		log.Println("No guide.json found, skipping")
		return false
	}

	file, err := os.Open(config.GuideFile)
	if err != nil {
		log.Printf("Error opening guide.json: %v", err)
		return false
	}
	defer file.Close() //nolint: errcheck

	var newGuideData map[string]interface{}
	if err := json.NewDecoder(file).Decode(&newGuideData); err != nil {
		log.Printf("Error decoding guide.json: %v", err)
		return false
	}

	guideDataMutex.Lock()
	guideData = newGuideData
	guideDataMutex.Unlock()

	programs, ok := guideData["programs"].([]interface{})
	if ok {
		log.Printf("Loaded guide data: %d entries", len(programs))
	}

	return true
}

func setupFileWatcher(filePath string) {
	var err error
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close() //nolint: errcheck

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					log.Println("Modified file detected:", event.Name)
					_ = loadGuide()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("Error:", err)
			}
		}
	}()

	err = watcher.Add(filePath)
	if err != nil {
		log.Printf("Error adding watcher for %s: %v", filePath, err)
		return
	}

	select {}
}

func getGuide(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	rows, err := dbQueryContext(ctx, "SELECT guide_number FROM channels WHERE enabled=1")
	if err != nil {
		log.Printf("Error fetching valid channels: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	channelMap := make(map[string]bool)
	for rows.Next() {
		var chNum string
		if err := rows.Scan(&chNum); err != nil {
			log.Printf("Error scanning channel number: %v", err)
			continue
		}
		channelMap[chNum] = true
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating channels for guide: %v", err)
	}

	var filteredPrograms []map[string]interface{}

	guideDataMutex.RLock()
	programs, ok := guideData["programs"].([]interface{})
	guideDataMutex.RUnlock()

	if ok {
		now := time.Now()
		for _, progInterface := range programs {
			progMap, ok := progInterface.(map[string]interface{})
			if !ok {
				continue
			}

			channelNum, ok := progMap["channel"].(string)
			if !ok || !channelMap[channelNum] {
				continue
			}

			endTimeStr, endOk := progMap["end"].(string)
			if !endOk {
				continue
			}

			endTime, err := time.Parse(time.RFC3339, endTimeStr)
			if err != nil {
				log.Printf("Error parsing end time: %v", err)
				continue
			}

			if endTime.Before(now) {
				continue
			}

			filteredPrograms = append(filteredPrograms, progMap)
		}

		sort.Slice(filteredPrograms, func(i, j int) bool {
			startI := filteredPrograms[i]["start"].(string)
			startJ := filteredPrograms[j]["start"].(string)
			channelI := filteredPrograms[i]["channel"].(string)
			channelJ := filteredPrograms[j]["channel"].(string)

			if startI != startJ {
				return startI < startJ
			}
			return channelI < channelJ
		})
	}

	newGuideData := make(map[string]interface{})
	newGuideData["programs"] = filteredPrograms
	newGuideData["channels"] = guideData["channels"]
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(newGuideData); err != nil {
		log.Printf("Error encoding guide response: %v", err)
	}
}

func cleanupOldRecordings() {
	log.Println("Cleaning up old recordings")
	loc, err := getLocalLocation()
	if err != nil {
		log.Printf("Error determining timezone: %v", err)
		loc = time.UTC
	}

	rows, err := dbQueryContext(context.Background(), `
        SELECT id, date, start_time, duration
        FROM recordings
        WHERE status IN ('pending', 'recording')
			`)
	if err != nil {
		log.Printf("Error loading recordings for cleanup: %v", err)
		return
	}

	// Collect all pending or stuck recording IDs and computed end times first.
	// We must close the SELECT cursor (releasing the SQLite read lock)
	// before doing any UPDATEs, otherwise SQLite will return "database is locked"
	// because a pending read cursor blocks writes.
	type recordingInfo struct {
		id      int
		endTime time.Time
	}
	var toUpdate []recordingInfo

	for rows.Next() {
		var id int
		var date, startTime string
		var duration int
		if err := rows.Scan(&id, &date, &startTime, &duration); err != nil {
			log.Printf("Error scanning recording: %v", err)
			continue
		}

		dateTimeStr := fmt.Sprintf("%s %s", date, startTime)
		startTimeParsed, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
		if err != nil {
			log.Printf("Error parsing start time for recording %d: %v", id, err)
			continue
		}

		adjustedStartTime := startTimeParsed.Add(-PreRollSeconds * time.Second)
		endTime := adjustedStartTime.Add(time.Duration(duration+PostRollMinutes) * time.Minute)

		toUpdate = append(toUpdate, recordingInfo{id, endTime})
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating recordings for cleanup: %v", err)
	}
	rows.Close() // nolint: errcheck — close cursor before writing to avoid SQLite lock

	now := time.Now().In(loc)
	updatedCount := 0

	for _, info := range toUpdate {
		if now.After(info.endTime) {
			_, err := dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", info.id)
			if err != nil {
				log.Printf("Error updating recording %d status to failed: %v", info.id, err)
			} else {
				log.Printf("Marked recording %d as failed (ended at %v)", info.id, info.endTime)
				updatedCount++
			}
		}
	}

	log.Printf("Cleaned up %d old recordings", updatedCount)
}

func isHttpServerError(logFile string) bool {
	content, err := os.ReadFile(logFile)
	if err != nil {
		return false

	}
	output := string(content)
	return strings.Contains(output, "HTTP error 503") ||
		strings.Contains(output, "Server returned 5XX Server Error")
}

type DiscoveryResponse struct {
	TunerCount int `json:"TunerCount"`
}

func fetchTunerCount() int {
	defaultCount := 4
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", "http://hdhomerun.local/discover.json", nil)
	if err != nil {
		log.Printf("Error creating tuner count request: %v, using default %d", err, defaultCount)
		cancel()
		return defaultCount
	}
	resp, err := http.DefaultClient.Do(req)
	cancel()
	if err != nil {
		log.Printf("Error fetching tuner count: %v, using default %d", err, defaultCount)
		return defaultCount
	}
	defer resp.Body.Close()

	var disc DiscoveryResponse
	if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
		log.Printf("Error decoding tuner count: %v, using default %d", err, defaultCount)
		return defaultCount
	}

	if disc.TunerCount <= 0 {
		log.Printf("Invalid TunerCount %d received, using default %d", disc.TunerCount, defaultCount)
		return defaultCount
	}

	return disc.TunerCount
}
