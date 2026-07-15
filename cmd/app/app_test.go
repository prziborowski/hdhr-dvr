package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	pkgcfg "github.com/prziborowski/hdhr-dvr/pkg/config"
	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

// --- Mocks ---

type MockCommander struct {
	RunCommandFunc   func(name string, args ...string) error
	StartCommandFunc func(name string, stdout, stderr io.Writer, args ...string) (*exec.Cmd, error)
	StatFunc         func(path string) (os.FileInfo, error)
	MkdirAllFunc     func(path string, perm os.FileMode) error
	RemoveFunc       func(path string) error
	CreateFunc       func(path string) (*os.File, error)
	OpenFunc         func(path string) (*os.File, error)
	ReadFileFunc     func(path string) ([]byte, error)
}

func (m *MockCommander) RunCommand(name string, args ...string) error {
	if m.RunCommandFunc != nil {
		return m.RunCommandFunc(name, args...)
	}
	return nil
}

func (m *MockCommander) StartCommand(name string, stdout, stderr io.Writer, args ...string) (*exec.Cmd, error) {
	if m.StartCommandFunc != nil {
		return m.StartCommandFunc(name, stdout, stderr, args...)
	}
	return &exec.Cmd{}, nil
}

func (m *MockCommander) Stat(path string) (os.FileInfo, error) {
	if m.StatFunc != nil {
		return m.StatFunc(path)
	}
	return nil, fmt.Errorf("file not found")
}

func (m *MockCommander) MkdirAll(path string, perm os.FileMode) error {
	if m.MkdirAllFunc != nil {
		return m.MkdirAllFunc(path, perm)
	}
	return nil
}

func (m *MockCommander) Remove(path string) error {
	if m.RemoveFunc != nil {
		return m.RemoveFunc(path)
	}
	return nil
}

func (m *MockCommander) Create(path string) (*os.File, error) {
	if m.CreateFunc != nil {
		return m.CreateFunc(path)
	}
	return nil, fmt.Errorf("cannot create file")
}

func (m *MockCommander) Open(path string) (*os.File, error) {
	if m.OpenFunc != nil {
		return m.OpenFunc(path)
	}
	return nil, fmt.Errorf("file not found")
}

func (m *MockCommander) ReadFile(path string) ([]byte, error) {
	if m.ReadFileFunc != nil {
		return m.ReadFileFunc(path)
	}
	return nil, fmt.Errorf("cannot read file")
}

// setupTestApp creates an App instance with an in-memory SQLite database.
func setupTestApp(t *testing.T) (*App, *sql.DB) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}

	cfg := &pkgcfg.Config{
		StorageDir: "/tmp/dvr_test",
		Timezone:   "UTC",
	}

	store := NewSQLStore(db)
	commander := &MockCommander{}
	app := NewApp(cfg, store, commander)
	app.tunerCount = 2

	// Initialize tables
	app.createTables()

	return app, db
}

// --- Tests ---

func TestIsTunerAvailable(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	// Setup: Insert some recordings to occupy tuners
	_, err := db.Exec("INSERT INTO recordings (channel_id, date, start_time, duration, status) VALUES (?, ?, ?, ?, ?)", "1", "2026-07-14", "12:00", 60, "pending")
	if err != nil {
		t.Fatal(err)
	}

	// Add second recording to fill tuners (tunerCount is 2)
	_, err = db.Exec("INSERT INTO recordings (channel_id, date, start_time, duration, status) VALUES (?, ?, ?, ?, ?)", "2", "2026-07-14", "12:30", 60, "pending")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		req      RecordingRequest
		expected bool
	}{
			{
			name:      "Tuner Available (No overlap)",
			req:      RecordingRequest{Date: "2026-07-14", StartTime: "14:00", Duration: 60},
			expected: true,
			},
			{
			name:      "Tuner Full (Overlap with both recordings)",
			req:      RecordingRequest{Date: "2026-07-14", StartTime: "12:45", Duration: 30},
			expected: false,
			},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := app.isTunerAvailable(context.Background(), tt.req)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != tt.expected {
				t.Errorf("got %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCleanupOldRecordings(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	// Set timezone to UTC for predictable tests
	app.config.Timezone = "UTC"

	now := time.Now().UTC()
	pastDate := now.Add(-24 * time.Hour).Format("2006-01-02")
	pastTime := now.Add(-24 * time.Hour).Format("15:04")

	// Recording that should be marked failed (ended yesterday)
	_, err := db.Exec("INSERT INTO recordings (channel_id, date, start_time, duration, status) VALUES (?, ?, ?, ?, ?)", "1", pastDate, pastTime, 60, "pending")
	if err != nil {
		t.Fatal(err)
	}

	// Recording that is still valid (starts tomorrow)
	futureDate := now.Add(24 * time.Hour).Format("2006-01-02")
	futureTime := now.Add(24 * time.Hour).Format("15:04")
	_, err = db.Exec("INSERT INTO recordings (channel_id, date, start_time, duration, status) VALUES (?, ?, ?, ?, ?)", "2", futureDate, futureTime, 60, "pending")
	if err != nil {
		t.Fatal(err)
	}

	app.cleanupOldRecordings()

	var status1 string
	err = db.QueryRow("SELECT status FROM recordings WHERE channel_id = '1'").Scan(&status1)
	if err != nil || status1 != "failed" {
		t.Errorf("expected recording 1 to be failed, got %s (err: %v)", status1, err)
	}

	var status2 string
	err = db.QueryRow("SELECT status FROM recordings WHERE channel_id = '2'").Scan(&status2)
	if err != nil || status2 != "pending" {
		t.Errorf("expected recording 2 to be pending, got %s (err: %v)", status2, err)
	}
}

func TestMarkFailed(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	_, err := db.Exec("INSERT INTO recordings (id, channel_id, date, start_time, duration, status) VALUES (123, '1', '2026-07-14', '12:00', 60, 'pending')")
	if err != nil {
		t.Fatal(err)
	}

	app.markFailed(123)

	var status string
	err = db.QueryRow("SELECT status FROM recordings WHERE id = 123").Scan(&status)
	if err != nil || status != "failed" {
		t.Errorf("expected status failed, got %s (err: %v)", status, err)
	}
}

func TestCreateRecordingHandler(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	// Setup channel
	_, err := db.Exec("INSERT INTO channels (guide_number, guide_name, url, enabled) VALUES (?, ?, ?, ?)", "101", "Test Channel", "http://test", 1)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		body         interface{}
		expectedCode int
	}{
		{
			name:         "Success",
			body:         RecordingRequest{ChannelID: "101", Date: "2026-07-14", StartTime: "12:00", Duration: 30},
			expectedCode: http.StatusCreated,
		},
		{
			name:         "Invalid Duration",
			body:         RecordingRequest{ChannelID: "101", Date: "2026-07-14", StartTime: "12:00", Duration: -1},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Channel Not Found",
			body:         RecordingRequest{ChannelID: "999", Date: "2026-07-14", StartTime: "12:00", Duration: 30},
			expectedCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest("POST", "/api/recordings", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			app.createRecording(rr, req)

			if rr.Code != tt.expectedCode {
				t.Errorf("got code %d, want %d", rr.Code, tt.expectedCode)
			}
		})
	}
}

func TestUpdateRecordingHandler(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	_, err := db.Exec("INSERT INTO recordings (id, channel_id, date, start_time, duration, status) VALUES (123, '1', '2026-07-14', '12:00', 60, 'pending')")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		id           string
		body         interface{}
		expectedCode int
	}{
		{
			name:         "Success",
			id:           "123",
			body:         map[string]string{"title": "New Title"},
			expectedCode: http.StatusNoContent,
		},
		{
			name:         "Not Found",
			id:           "456",
			body:         map[string]string{"title": "New Title"},
			expectedCode: http.StatusNotFound,
		},
		{
			name:         "Invalid ID",
			id:           "abc",
			body:         map[string]string{"title": "New Title"},
			expectedCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest("PATCH", "/api/recordings/"+tt.id, bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")

			// Instead, we use a real router for handler tests that depend on mux vars
			r := mux.NewRouter()
			r.HandleFunc("/api/recordings/{id}", app.updateRecording).Methods("PATCH")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tt.expectedCode {
				t.Errorf("got code %d, want %d", rr.Code, tt.expectedCode)
			}
		})
	}
}

func TestDeleteRecordingHandler(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	_, err := db.Exec("INSERT INTO recordings (id, channel_id, date, start_time, duration, status) VALUES (123, '1', '2026-07-14', '12:00', 60, 'pending')")
	if err != nil {
		t.Fatal(err)
	}

	r := mux.NewRouter()
	r.HandleFunc("/api/recordings/{id}", app.deleteRecording).Methods("DELETE")

	req := httptest.NewRequest("DELETE", "/api/recordings/123", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("got code %d, want %d", rr.Code, http.StatusNoContent)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM recordings WHERE id = 123").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("recording was not deleted from db")
	}
}

func TestGetRecordingsHandler(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	_, err := db.Exec("INSERT INTO channels (guide_number, guide_name) VALUES (?, ?)", "101", "Test Channel")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO recordings (channel_id, date, start_time, duration, status, title) VALUES (?, ?, ?, ?, ?, ?)", "101", "2026-07-14", "12:00", 60, "pending", "Test Rec")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/recordings", nil)
	rr := httptest.NewRecorder()
	app.getRecordings(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("got code %d, want %d", rr.Code, http.StatusOK)
	}

	var res []GetRecordingsRec
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	if len(res) != 1 || res[0].Title == nil || *res[0].Title != "Test Rec" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestGetKeywordsHandler(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	_, err := db.Exec("INSERT INTO keywords (name, category, enabled) VALUES (?, ?, ?)", "test-word", "sports", 1)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/keywords", nil)
	rr := httptest.NewRecorder()
	app.getKeywords(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("got code %d, want %d", rr.Code, http.StatusOK)
	}

	var res []types.Keyword
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	if len(res) != 1 || res[0].Name != "test-word" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestConvertToMp4(t *testing.T) {
	commander := &MockCommander{}
	var capturedArgs []string
	commander.StartCommandFunc = func(name string, stdout, stderr io.Writer, args ...string) (*exec.Cmd, error) {
		capturedArgs = args
		return &exec.Cmd{}, nil
	}

	// Since we are mocking StartCommand to return a dummy *exec.Cmd, and that Cmd will fail on Wait(),
	// the function will try the slower conversion as well.
	err := convertToMp4(commander, "input.ts", "output.mp4")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(capturedArgs) == 0 {
		t.Error("ffmpeg was never called")
	}
}
