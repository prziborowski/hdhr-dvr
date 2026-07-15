package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	preRollSeconds   = 30
	postRollMinutes  = 1
	hdhomerunBaseURL = "http://hdhomerun.local"
	queryTimeout     = 10 * time.Second
)

// recordingCh is used to queue new recordings for the scheduler.
var recordingCh = make(chan types.Recording, 100)

// App holds all dependencies for the HDHomeRun DVR application.
// App holds all dependencies for the HDHomeRun DVR application.
type App struct {
	store               types.Store
	sqlDB               *sql.DB
	config                *pkgcfg.Config
	tunerCount          int
	guideData           types.Guide
	guideDataMutex      sync.RWMutex
	watcher               *fsnotify.Watcher
	runningProcesses    sync.Map // key: recording ID, value: *exec.Cmd
	enabledChannels     map[string]bool
	enabledChannelsMutex sync.RWMutex
}

func main() {
	app := &App{
		enabledChannels: make(map[string]bool),
	}

	// Initialize database
	var err error
	app.sqlDB, err = sql.Open("sqlite3", "./recordings.db")
	if err != nil {
		log.Fatal(err)
	}
	defer app.sqlDB.Close() //nolint: errcheck

	app.store = types.NewStoreAdapter(app.sqlDB)
	app.sqlDB.SetMaxOpenConns(10)
	app.sqlDB.SetMaxIdleConns(5)
	app.sqlDB.SetConnMaxLifetime(0)

	// Load configuration
	app.config, err = pkgcfg.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	app.tunerCount = app.fetchTunerCount()
	log.Printf("System initialized with %d tuners", app.tunerCount)

	app.createTables()
	app.loadEnabledChannels()
	app.loadChannels()

	if app.loadGuide() {
		go app.setupFileWatcher(app.config.GuideFile)
	}

	app.loadRecordings()
	app.cleanupOldRecordings()

	go app.startRecordingScheduler()

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			app.cleanupOldRecordings()
		}
	}()

	r := mux.NewRouter()

	r.HandleFunc("/", app.serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/schedule", app.serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/recordings", app.serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/guide", app.serveHome).Methods("GET", "HEAD")
	r.HandleFunc("/keywords", app.serveHome).Methods("GET", "HEAD")

	r.HandleFunc("/api/channels", app.getChannels).Methods("GET")
	r.HandleFunc("/api/recordings", app.getRecordings).Methods("GET")
	r.HandleFunc("/api/recordings", app.createRecording).Methods("POST")
	r.HandleFunc("/api/recordings/{id}", app.deleteRecording).Methods("DELETE")
	r.HandleFunc("/api/recordings/{id}", app.updateRecording).Methods("PATCH")
	r.HandleFunc("/api/recordings/{id}/file", app.getRecordingFile).Methods("GET", "HEAD")
	r.HandleFunc("/api/guide", app.getGuide).Methods("GET")
	r.HandleFunc("/api/keywords", app.getKeywords).Methods("GET")
	r.HandleFunc("/api/keywords", app.createKeyword).Methods("POST")
	r.HandleFunc("/api/keywords/{id}", app.deleteKeyword).Methods("DELETE")

	log.Println("Server starting on :8080...")
	server := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		log.Println("\nReceived shutdown signal...")

		activeCount := 0
		app.runningProcesses.Range(func(key, value interface{}) bool {
			activeCount++
			return true
		})

		if activeCount > 0 {
			log.Printf("WARNING: %d recording(s) in progress. Terminating...", activeCount)

			app.runningProcesses.Range(func(key, value interface{}) bool {
				if cmd, ok := value.(*exec.Cmd); ok && cmd.Process != nil {
					log.Printf("Terminating recording %d...", key)
					if err := cmd.Process.Kill(); err != nil {
						log.Printf("Error killing recording %d: %v", key, err)
					}
				}
				return true
			})

			time.Sleep(2 * time.Second)
			log.Println("Recordings terminated. Proceeding with shutdown...")
		}

		log.Println("No active recordings. Shutting down gracefully...")
		time.Sleep(1 * time.Second)

		if err := server.Shutdown(context.Background()); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	log.Fatal(server.ListenAndServe())
}

// ---------------------------------------------------------------------------
// Recording creation handler
// ---------------------------------------------------------------------------

func (a *App) updateRecording(w http.ResponseWriter, r *http.Request) {
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

	result, err := a.dbExecContext(ctx, "UPDATE recordings SET title = ? WHERE id = ? AND status = 'pending'", *updateReq.Title, id)
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

func (a *App) createRecording(w http.ResponseWriter, r *http.Request) {
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

	if req.Duration <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Duration must be positive"}) //nolint: errcheck
		return
	}

	var exists bool
	err := a.dbQueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM channels WHERE guide_number = ?)", req.ChannelID).Scan(&exists)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Channel not found"}) //nolint: errcheck
		return
	}

	var duplicateExists bool
	err = a.dbQueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM recordings WHERE channel_id = ? AND date = ? AND start_time = ?)", req.ChannelID, req.Date, req.StartTime).Scan(&duplicateExists)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if duplicateExists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "Recording already exists for this channel and time"}) //nolint: errcheck
		return
	}

	// Validate tuner availability with the computed time window
	if _, err := a.isTunerAvailable(ctx, req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	txCtx, txCancel := context.WithTimeout(ctx, 5*time.Second)
	defer txCancel()
	tx, err := a.store.BeginTx(txCtx, nil)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint: errcheck

	recording := types.Recording{
		ChannelID: req.ChannelID,
		Date:      req.Date,
		StartTime: req.StartTime,
		Duration:  req.Duration,
		Status:    "pending",
		Title:     req.Title,
	}

	result, err := tx.ExecContext(ctx, `
        INSERT INTO recordings (channel_id, date, start_time, duration, status, title)
        VALUES (?, ?, ?, ?, ?, ?)
     `, recording.ChannelID, recording.Date, recording.StartTime, recording.Duration, recording.Status, recording.Title)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to create recording"}) //nolint: errcheck
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to get recording ID"}) //nolint: errcheck
		return
	}
	recording.ID = int(id)

	if err := tx.Commit(); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		tx.Rollback() //nolint: errcheck
		return
	}

	recordingCh <- recording

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(recording) //nolint: errcheck
}

// isTunerAvailable returns true if there are fewer active tuners than the configured limit
// during the requested time window.
func (a *App) isTunerAvailable(ctx context.Context, req RecordingRequest) (bool, error) {
	startStr := req.Date + " " + req.StartTime
	reqStart, err := time.Parse("2006-01-02 15:04", startStr)
	if err != nil {
		return false, err
	}
	reqEnd := reqStart.Add(time.Duration(req.Duration) * time.Minute)

	rows, err := a.dbQueryContext(ctx, `
		SELECT date, start_time, duration
		FROM recordings
		WHERE datetime(date || ' ' || start_time) < datetime(?, '+' || ? || ' minutes')
		  AND datetime(date || ' ' || start_time, '+' || duration || ' minutes') > datetime(?)`,
		startStr, req.Duration, startStr)
	if err != nil {
		return false, err
	}
	defer rows.Close() // nolint: errcheck

	type event struct {
		t    time.Time
		diff int
	}
	var events []event
	for rows.Next() {
		var d, s string
		var dur int
		if err := rows.Scan(&d, &s, &dur); err != nil {
			return false, err
		}
		st, _ := time.Parse("2006-01-02 15:04", d+" "+s)
		et := st.Add(time.Duration(dur) * time.Minute)
		events = append(events, event{st, 1})
		events = append(events, event{et, -1})
	}

	sort.Slice(events, func(i, j int) bool {
		if events[i].t.Equal(events[j].t) {
			return events[i].diff < events[j].diff
		}
		return events[i].t.Before(events[j].t)
	})

	currentTuners := 0
	for _, e := range events {
		currentTuners += e.diff
		if (e.t.After(reqStart) || e.t.Equal(reqStart)) && (e.t.Before(reqEnd) || e.t.Equal(reqEnd)) {
			if currentTuners >= a.tunerCount-1 {
				return false, nil
			}
		}
	}

	return true, nil
}

// ---------------------------------------------------------------------------
// Recording file serving
// ---------------------------------------------------------------------------

func (a *App) getRecordingFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	idStr := mux.Vars(r)["id"]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid recording ID", http.StatusBadRequest)
		return
	}

	var recording types.Recording
	var channelName string
	err = a.dbQueryRowContext(ctx, `
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

	if recording.Status != "completed" {
		http.Error(w, "Recording not completed", http.StatusForbidden)
		return
	}

	// After conversion completes, the original .ts is deleted and only .mp4 remains.
	// Build the file path with .mp4 extension to match what's actually on disk.
	originalPath := filepath.Join(a.config.StorageDir, recording.GetFilePath())
	outputFile := strings.TrimSuffix(originalPath, filepath.Ext(originalPath)) + ".mp4"

	// Serve the file using http.ServeContent which handles Range requests,
	// Content-Type detection, and Content-Length automatically.
	http.ServeFile(w, r, outputFile)
}

// ---------------------------------------------------------------------------
// Recording scheduler & deletion
// ---------------------------------------------------------------------------

func (a *App) deleteRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	idStr := mux.Vars(r)["id"]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid recording ID", http.StatusBadRequest)
		return
	}

	_, err = a.dbExecContext(ctx, "DELETE FROM recordings WHERE id = ?", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	recordingTimers.Delete(id)

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Database helpers & getters/setters
// ---------------------------------------------------------------------------

func (a *App) dbQueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return a.store.QueryContext(ctx, query, args...)
}

func (a *App) dbExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return a.store.ExecContext(ctx, query, args...)
}

func (a *App) dbQueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return a.store.QueryRowContext(ctx, query, args...)
}

func (a *App) markFailed(id int) {
	log.Printf("Marking recording %d as failed", id)
	_, err := a.dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", id)
	if err != nil {
		log.Printf("Error updating recording status to failed: %v", err)
	}
}

// getLocalLocation returns the configured timezone location.
func (a *App) getLocalLocation() (*time.Location, error) {
	tz := "America/Los_Angeles"
	if a.config != nil && a.config.Timezone != "" {
		tz = a.config.Timezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC, nil
	}
	return loc, nil
}

// ---------------------------------------------------------------------------
// Database initialization & data loading
// ---------------------------------------------------------------------------

func (a *App) createTables() {
	_, err := a.store.ExecContext(context.Background(), `
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

func (a *App) loadEnabledChannels() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := a.dbQueryContext(ctx, "SELECT guide_number FROM channels WHERE enabled=1")
	if err != nil {
		log.Printf("Error loading enabled channels: %v", err)
		return
	}
	defer rows.Close() //nolint:errcheck

	newEnabledChannels := make(map[string]bool)
	for rows.Next() {
		var chNum string
		if err := rows.Scan(&chNum); err != nil {
			log.Printf("Error scanning enabled channel number: %v", err)
			continue
		}
		newEnabledChannels[chNum] = true
	}

	a.enabledChannelsMutex.Lock()
	a.enabledChannels = newEnabledChannels
	a.enabledChannelsMutex.Unlock()
}

func (a *App) loadChannels() {
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

	tx, err := a.store.BeginTx(context.Background(), nil)
	if err != nil {
		log.Printf("Error starting transaction for channels: %v", err)
		return
	}

	_, err = tx.ExecContext(context.Background(), "UPDATE channels SET enabled=0")
	if err != nil {
		log.Printf("Error clearing channels table: %v", err)
		tx.Rollback() //nolint: errcheck
		return
	}

	failedCount := 0
	for _, ch := range chs {
			_, err := tx.ExecContext(context.Background(), "INSERT OR REPLACE INTO channels (guide_number, guide_name, url, enabled) VALUES (?, ?, ?, ?)",
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
	a.loadEnabledChannels()
}

func (a *App) loadRecordings() {
	log.Println("Load recordings")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := a.store.BeginTx(ctx, nil)
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

	loc, _ := a.getLocalLocation()
	now := time.Now().In(loc)

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

		adjustedStartTime := startTime.Add(-preRollSeconds * time.Second)
		endTime := adjustedStartTime.Add(time.Duration(r.Duration+postRollMinutes) * time.Minute)

		if now.After(endTime) {
			log.Printf("Skipping recording %d - already ended at %v", r.ID, endTime)
			continue
		}

		newStatus := r.CheckStatus(a.store, loc, a.config.StorageDir)
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
			go a.startRecording(r)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating recordings: %v", err)
	}
	if err := rows.Close(); err != nil {
		log.Printf("Error closing recordings cursor: %v", err)
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing transaction: %v", err)
		tx.Rollback() //nolint: errcheck
		return
	}
}

// ---------------------------------------------------------------------------
// Recording scheduler
// ---------------------------------------------------------------------------

var recordingTimers sync.Map // key: recording ID, value: struct{}

func (a *App) startRecordingScheduler() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	loc, err := a.getLocalLocation()
	if err != nil {
		log.Printf("Error determining timezone: %v", err)
		loc = time.UTC
	}

	for {
		select {
		case <-ticker.C:
			now := time.Now().In(loc)
			rows, err := a.store.QueryContext(context.Background(), `
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

			for _, r := range recordings {
				if _, exists := recordingTimers.Load(r.ID); exists {
					continue
				}

				startTime, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%s %s", r.Date, r.StartTime), loc)
				if err != nil {
					log.Printf("Error parsing start time for recording %d: %v", r.ID, err)
					continue
				}

				actualStartTime := startTime.Add(-preRollSeconds * time.Second)

				if now.Before(actualStartTime) {
					go a.startRecordingTimer(r, actualStartTime)
				} else if now.Before(startTime.Add(time.Duration(r.Duration+postRollMinutes) * time.Minute)) {
					log.Printf("Recording %d should have started at %v, starting now", r.ID, actualStartTime)
					go a.startRecording(r)
				} else {
					log.Printf("Recording %d should have started at %v but it's now %v, marking as failed",
						r.ID, actualStartTime, now)
					a.markFailed(r.ID)
				}
			}
		case <-recordingCh:
		}
	}
}

func (a *App) startRecordingTimer(recording types.Recording, startTime time.Time) {
	recordingTimers.Store(recording.ID, struct{}{})
	defer recordingTimers.Delete(recording.ID)

	duration := time.Until(startTime)

	if duration > 0 {
		time.Sleep(duration)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var exists bool
	err := a.dbQueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM recordings WHERE id = ? AND status = 'pending')", recording.ID).Scan(&exists)
	cancel()
	if err != nil {
		log.Printf("Error checking if recording %d exists: %v", recording.ID, err)
		return
	}

	if !exists {
		log.Printf("Recording %d was deleted before start time, not starting", recording.ID)
		return
	}

	log.Printf("Starting recording %d at %v (scheduled for %v)",
		recording.ID, time.Now(), startTime.Add(preRollSeconds*time.Second))
	go a.startRecording(recording)
}

// ---------------------------------------------------------------------------
// startRecording entry point — validates channel and orchestrates the process.
// ---------------------------------------------------------------------------

func (a *App) startRecording(r types.Recording) {
	ch, err := a.getChannelInfo(r.ChannelID)
	if err != nil {
		log.Printf("Error finding channel %s: %v", r.ChannelID, err)
		a.markFailed(r.ID)
		return
	}

	loc, err := a.getLocalLocation()
	if err != nil {
		log.Printf("Error determining timezone: %v", err)
	}

	dateTimeStr := fmt.Sprintf("%s %s", r.Date, r.StartTime)
	startTime, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
	if err != nil {
		log.Printf("Error parsing start time: %v", err)
		a.markFailed(r.ID)
		return
	}

	adjustedStartTime := startTime.Add(-preRollSeconds * time.Second)
	adjustedDuration := r.Duration + postRollMinutes

	log.Printf("Original start time: %v, Adjusted start time: %v, Original duration: %d, Adjusted duration: %d",
		startTime, adjustedStartTime, r.Duration, adjustedDuration)

	if err := os.MkdirAll(a.config.StorageDir, 0755); err != nil {
		log.Printf("Error creating output directory: %v", err)
		a.markFailed(r.ID)
		return
	}

	outputFile := filepath.Join(a.config.StorageDir, r.GetFilePath())
	logFile := filepath.Join("/tmp", fmt.Sprintf("ffmpeg-%s-%s.log", r.Date, r.StartTime))
	logFileHandle, err := os.Create(logFile)
	if err != nil {
		log.Printf("Error creating log file: %v", err)
		_, updateErr := a.dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", r.ID)
		if updateErr != nil {
			log.Printf("Error updating recording status: %v", updateErr)
		}
		return
	}
	defer logFileHandle.Close() //nolint: errcheck

	durationSeconds := adjustedDuration * 60
	ffmpegArgs := buildFFmpegArgs(ch.URL, durationSeconds, outputFile)
	cmd := exec.Command("ffmpeg", ffmpegArgs...)

	log.Printf("Starting recording: %s", outputFile)
	log.Printf("Channel: %s (%s)", ch.GuideName, ch.GuideNumber)
	log.Printf("Original Date: %s, Original Time: %s, Adjusted Time: %v, Duration: %d minutes (original: %d)",
		r.Date, r.StartTime, adjustedStartTime.Format("15:04"), adjustedDuration, r.Duration)
	log.Printf("Storage directory: %s", a.config.StorageDir)
	log.Printf("Log file: %s", logFile)
	log.Printf("FFmpeg command: %s", getFFmpegCommandString(ch.URL, durationSeconds, outputFile))

	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle

	if err := a.updateStatusWithRetry(r.ID, "recording"); err != nil {
		logFileHandle.Close() //nolint: errcheck
		return
	}

	a.runningProcesses.Store(r.ID, cmd)
	defer a.runningProcesses.Delete(r.ID)

	var runErr error
	retryCount := 0
	maxRetries := 3
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

			ffmpegArgs := buildFFmpegArgs(ch.URL, durationSeconds, outputFile)
			cmd = exec.Command("ffmpeg", ffmpegArgs...)
			cmd.Stdout = logFileHandle
			cmd.Stderr = logFileHandle
			a.runningProcesses.Store(r.ID, cmd)

			retryCount++
			continue
		}
		break
	}

	if runErr != nil {
		log.Printf("Error running ffmpeg after retries: %v", runErr)
		if _, err := os.Stat(outputFile); err == nil {
			a.updateStatusWithRetry(r.ID, "completed") //nolint:errcheck
		} else {
			a.markFailed(r.ID)
		}
		return
	}

	if err := a.updateStatusWithRetry(r.ID, "completed"); err != nil {
		return
	}

	mp4File := strings.TrimSuffix(outputFile, filepath.Ext(outputFile)) + ".mp4"
	if err := convertToMp4(outputFile, mp4File); err != nil {
		log.Printf("Conversion warning: %v", err)
	} else {
		_ = os.Remove(outputFile)
		if info, err := os.Stat(mp4File); err == nil {
			size := info.Size()
			_, updateErr := a.dbExecContext(context.Background(), "UPDATE recordings SET file_size = ? WHERE id = ?", size, r.ID)
			if updateErr != nil {
				log.Printf("Error updating recording file size: %v", updateErr)
			}
		} else {
			log.Printf("Error getting final recording file size: %v", err)
		}
	}

	log.Printf("Recording completed successfully and converted to MP4: %s", mp4File)
}

// getChannelInfo validates the channel exists and returns its details.
func (a *App) getChannelInfo(channelID string) (types.Channel, error) {
	var ch types.Channel
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.dbQueryRowContext(ctx, "SELECT guide_number, guide_name, url FROM channels WHERE guide_number = ?", channelID).Scan(
		&ch.GuideNumber, &ch.GuideName, &ch.URL)
	return ch, err
}

// updateStatusWithRetry updates a recording status with exponential backoff.
func (a *App) updateStatusWithRetry(id int, status string) error {
	retryCount := 0
	maxRetries := 3

	for retryCount < maxRetries {
		txCtx, txCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer txCancel()
	tx, err := a.store.BeginTx(txCtx, nil)
		if err != nil {
			log.Printf("Error starting database transaction: %v", err)
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			a.markFailed(id) //nolint:errcheck
			return err
		}

		_, err = tx.ExecContext(context.Background(), "UPDATE recordings SET status = ? WHERE id = ?", status, id)
		if err != nil {
			log.Printf("Error updating recording status to '%s': %v", status, err)
			tx.Rollback() //nolint: errcheck
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			a.markFailed(id) //nolint:errcheck
			return err
		}

		if err := tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			tx.Rollback() //nolint: errcheck
			retryCount++
			if retryCount < maxRetries {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			a.markFailed(id) //nolint:errcheck
			return err
		}
		return nil
	}
	return fmt.Errorf("failed to update status after %d retries", maxRetries)
}

// ---------------------------------------------------------------------------
// File watching & guide loading
// ---------------------------------------------------------------------------

func (a *App) loadGuide() bool {
	if _, err := os.Stat(a.config.GuideFile); os.IsNotExist(err) {
		log.Println("No guide.json found, skipping")
		return false
	}

	file, err := os.Open(a.config.GuideFile)
	if err != nil {
		log.Printf("Error opening guide.json: %v", err)
		return false
	}
	defer file.Close() //nolint: errcheck

	var newGuideData types.Guide
	if err := json.NewDecoder(file).Decode(&newGuideData); err != nil {
		log.Printf("Error decoding guide.json: %v", err)
		return false
	}

	a.guideDataMutex.Lock()
	a.guideData = newGuideData
	a.guideDataMutex.Unlock()

	log.Printf("Loaded guide data: %d programs", len(newGuideData.Programs))
	return true
}

func (a *App) setupFileWatcher(filePath string) {
	var err error
	a.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer a.watcher.Close() //nolint: errcheck

	go func() {
		for {
			select {
			case event, ok := <-a.watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					log.Println("Modified file detected:", event.Name)
					a.loadGuide() //nolint:errcheck
				}
			case err, ok := <-a.watcher.Errors:
				if !ok {
					return
				}
				log.Println("Error:", err)
			}
		}
	}()

	err = a.watcher.Add(filePath)
	if err != nil {
		log.Printf("Error adding watcher for %s: %v", filePath, err)
		return
	}

	select {}
}

// ---------------------------------------------------------------------------
// Status cleanup
// ---------------------------------------------------------------------------

func (a *App) cleanupOldRecordings() {
	log.Println("Cleaning up old recordings")
	loc, err := a.getLocalLocation()
	if err != nil {
		log.Printf("Error determining timezone: %v", err)
	}

	rows, err := a.dbQueryContext(context.Background(), `
         SELECT id, date, start_time, duration
         FROM recordings
         WHERE status IN ('pending', 'recording')
				`)
	if err != nil {
		log.Printf("Error loading recordings for cleanup: %v", err)
		return
	}

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

		adjustedStartTime := startTimeParsed.Add(-preRollSeconds * time.Second)
		endTime := adjustedStartTime.Add(time.Duration(duration+postRollMinutes) * time.Minute)

		toUpdate = append(toUpdate, recordingInfo{id, endTime})
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating recordings for cleanup: %v", err)
	}
	rows.Close() //nolint:errcheck

	now := time.Now().In(loc)
	updatedCount := 0

	for _, info := range toUpdate {
		if now.After(info.endTime) {
			_, err := a.dbExecContext(context.Background(), "UPDATE recordings SET status = 'failed' WHERE id = ?", info.id)
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

// ---------------------------------------------------------------------------
// Data API handlers
// ---------------------------------------------------------------------------

func (a *App) serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "templates/index.html")
}

func (a *App) getChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := a.dbQueryContext(ctx, "SELECT guide_number, guide_name FROM channels WHERE enabled=1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close() // nolint: errcheck

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

func (a *App) getRecordings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := a.dbQueryContext(ctx, `
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
	defer rows.Close() // nolint: errcheck

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

func (a *App) getGuide(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	a.enabledChannelsMutex.RLock()
	channelMap := make(map[string]bool)
	for k, v := range a.enabledChannels {
		channelMap[k] = v
	}
	a.enabledChannelsMutex.RUnlock()

	now := time.Now()

	a.guideDataMutex.RLock()
	programs := make([]types.Program, len(a.guideData.Programs))
	copy(programs, a.guideData.Programs)
	channels := make([]types.LineupData, len(a.guideData.Channels))
	copy(channels, a.guideData.Channels)
	a.guideDataMutex.RUnlock()

	var filteredPrograms []types.Program
	for _, prog := range programs {
		if !channelMap[prog.Channel] {
			continue
		}
		endTime, err := time.Parse(time.RFC3339, prog.End)
		if err != nil {
			log.Printf("Error parsing end time for program %q: %v", prog.Title, err)
			continue
		}
		if endTime.Before(now) {
			continue
		}
		filteredPrograms = append(filteredPrograms, prog)
	}

	sort.SliceStable(filteredPrograms, func(i, j int) bool {
		if filteredPrograms[i].Start == filteredPrograms[j].Start {
			return filteredPrograms[i].Channel < filteredPrograms[j].Channel
		}
		return filteredPrograms[i].Start < filteredPrograms[j].Start
	})

	resp := types.Guide{
		Channels:  channels,
		Programs:  filteredPrograms,
		Generated: a.guideData.Generated,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Error encoding guide response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Keyword handlers
// ---------------------------------------------------------------------------

func (a *App) getKeywords(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := a.dbQueryContext(ctx, "SELECT id, name, category, enabled, created_at FROM keywords ORDER BY created_at DESC")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close() // nolint: errcheck

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

func (a *App) createKeyword(w http.ResponseWriter, r *http.Request) {
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

	result, err := a.store.ExecContext(context.Background(), "INSERT INTO keywords (name, category, enabled) VALUES (?, ?, ?)", req.Name, req.Category, enabled)
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

func (a *App) deleteKeyword(w http.ResponseWriter, r *http.Request) {
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

	result, err := a.store.ExecContext(context.Background(), "DELETE FROM keywords WHERE id = ?", id)
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

// ---------------------------------------------------------------------------
// Helper functions (ffmpeg, discovery, etc.)
// ---------------------------------------------------------------------------

func convertToMp4(tsFile, mp4File string) error {
	log.Printf("Converting %s to %s...", tsFile, mp4File)
	args := []string{
		"-i", tsFile,
		"-c", "copy",
		"-movflags", "+faststart",
		"-y",
		mp4File,
	}
	cmd := exec.Command("ffmpeg", args...)
	if err := cmd.Run(); err != nil {
		log.Printf("ffmpeg conversion failed: %v, attempting slower conversion", err)

		args = []string{
			"-err_detect", "ignore_err",
			"-fflags", "+genpts+discardcorrupt",
			"-i", tsFile,
			"-c", "copy",
			"-map", "0",
			"-f", "matroska",
			"-y",
			mp4File,
		}
		cmd = exec.Command("ffmpeg", args...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg conversion failed: %w", err)
		}
	}
	return nil
}

func buildFFmpegArgs(inputURL string, durationSeconds int, outputFile string) []string {
	args := []string{
		"-i", inputURL,
		"-fflags", "+genpts",
		"-analyzeduration", "100M",
		"-probesize", "100M",

		"-ignore_io_errors", "1",
		"-err_detect", "ignore_err",
		"-max_interleave_delta", "100M",

		"-rtsp_transport", "tcp",
		"-reconnect", "1",
		"-reconnect_at_eof", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "600",

		"-t", fmt.Sprintf("%d", durationSeconds),
		"-c", "copy",
		"-f", "mpegts",
		outputFile,
	}
	return args
}

func getFFmpegCommandString(inputURL string, durationSeconds int, outputFile string) string {
	args := buildFFmpegArgs(inputURL, durationSeconds, outputFile)
	cmd := "ffmpeg " + strings.Join(args, " ")
	return cmd
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

func (a *App) fetchTunerCount() int {
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
	defer resp.Body.Close() // nolint: errcheck

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

type DiscoveryResponse struct {
	TunerCount int `json:"TunerCount"`
}
