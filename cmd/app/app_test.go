package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

// fakeFileInfo is a minimal os.FileInfo backed by a name and size.
type fakeFileInfo struct {
	name string
	size int64
}

func (f *fakeFileInfo) Name() string       { return f.name }
func (f *fakeFileInfo) Size() int64        { return f.size }
func (f *fakeFileInfo) Mode() fs.FileMode  { return 0 }
func (f *fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f *fakeFileInfo) IsDir() bool        { return false }
func (f *fakeFileInfo) Sys() interface{}   { return nil }

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
			name:     "Tuner Available (No overlap)",
			req:      RecordingRequest{Date: "2026-07-14", StartTime: "14:00", Duration: 60},
			expected: true,
		},
		{
			name:     "Tuner Full (Overlap with both recordings)",
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

func TestConvertRecordingHandler(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	// A completed recording whose .ts still exists on disk.
	_, err := db.Exec("INSERT INTO recordings (id, channel_id, date, start_time, duration, status, title) VALUES (?, ?, ?, ?, ?, ?, ?)",
		7, "101", "2026-07-14", "12:00", 60, "completed", "Show")
	if err != nil {
		t.Fatal(err)
	}

	tsPath := "/tmp/dvr_test/2026-07-14-12:00-Show.ts"
	mp4Path := "/tmp/dvr_test/2026-07-14-12:00-Show.mp4"

	// Fake on-disk state: the .ts exists, ffmpeg produces the .mp4.
	files := map[string]int64{tsPath: 12345}
	var removed []string

	commander := app.commander.(*MockCommander)
	commander.RunCommandFunc = func(name string, args ...string) error {
		files[mp4Path] = 67890
		delete(files, tsPath)
		return nil
	}
	commander.StatFunc = func(path string) (os.FileInfo, error) {
		if size, ok := files[path]; ok {
			return &fakeFileInfo{name: path, size: size}, nil
		}
		return nil, os.ErrNotExist
	}
	commander.RemoveFunc = func(path string) error {
		removed = append(removed, path)
		delete(files, path)
		return nil
	}

	r := mux.NewRouter()
	r.HandleFunc("/api/recordings/{id}/convert", app.convertRecordingHandler).Methods("POST")

	req := httptest.NewRequest("POST", "/api/recordings/7/convert", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got code %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp struct {
		ID        int   `json:"id"`
		Converted bool  `json:"converted"`
		FileSize  int64 `json:"file_size"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Converted || resp.ID != 7 || resp.FileSize != 67890 {
		t.Errorf("unexpected response: %+v", resp)
	}

	var size int
	if err := db.QueryRow("SELECT file_size FROM recordings WHERE id = 7").Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != 67890 {
		t.Errorf("expected persisted file_size 67890, got %d", size)
	}

	if len(removed) != 1 || removed[0] != tsPath {
		t.Errorf("expected .ts to be removed, got %v", removed)
	}
}

func TestConvertRecordingHandlerNotCompleted(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	_, err := db.Exec("INSERT INTO recordings (id, channel_id, date, start_time, duration, status, title) VALUES (?, ?, ?, ?, ?, ?, ?)",
		8, "101", "2026-07-14", "12:00", 60, "recording", "Live")
	if err != nil {
		t.Fatal(err)
	}

	r := mux.NewRouter()
	r.HandleFunc("/api/recordings/{id}/convert", app.convertRecordingHandler).Methods("POST")

	req := httptest.NewRequest("POST", "/api/recordings/8/convert", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("got code %d, want %d; body: %s", rr.Code, http.StatusConflict, rr.Body.String())
	}
}

func postGuide(t *testing.T, app *App, guide types.Guide) {
	t.Helper()
	body, err := json.Marshal(guide)
	if err != nil {
		t.Fatalf("failed to marshal guide: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/guide", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	app.setGuide(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("setGuide returned code %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestSetGuideHandler(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	guide := types.Guide{
		Channels: []types.LineupData{
			{StationID: "1", ChannelNumber: "101", StationCallSign: "KABC", Logo: "abc.png"},
			{StationID: "2", ChannelNumber: "202", StationCallSign: "KXTV", Logo: "xtv.png"},
		},
		Programs: []types.Program{
			{Channel: "101", Title: "News", SubTitle: "Morning", Start: "2026-01-01T00:00:00Z", End: "2026-01-01T01:00:00Z", Duration: 60, Category: "news"},
			{Channel: "202", Title: "Movie", Start: "2026-01-01T02:00:00Z", End: "2026-01-01T04:00:00Z", Duration: 120, Category: "movie", New: true},
		},
		Generated: "2026-01-01T00:00:00Z",
	}

	postGuide(t, app, guide)

	var chCount, progCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM guide_channels").Scan(&chCount); err != nil {
		t.Fatal(err)
	}
	if chCount != 2 {
		t.Errorf("expected 2 guide_channels, got %d", chCount)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM guide_programs").Scan(&progCount); err != nil {
		t.Fatal(err)
	}
	if progCount != 2 {
		t.Errorf("expected 2 guide_programs, got %d", progCount)
	}

	var generated string
	if err := db.QueryRow("SELECT generated FROM guide_meta WHERE id = 1").Scan(&generated); err != nil {
		t.Fatal(err)
	}
	if generated != "2026-01-01T00:00:00Z" {
		t.Errorf("expected generated %q, got %q", "2026-01-01T00:00:00Z", generated)
	}

	if len(app.guideData.Channels) != 2 {
		t.Errorf("expected in-memory 2 channels, got %d", len(app.guideData.Channels))
	}
	if len(app.guideData.Programs) != 2 {
		t.Errorf("expected in-memory 2 programs, got %d", len(app.guideData.Programs))
	}
}

func TestSetGuideReplaces(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	g1 := types.Guide{
		Channels:  []types.LineupData{{StationID: "1", ChannelNumber: "101"}},
		Programs:  []types.Program{{Channel: "101", Title: "A", Start: "2026-01-01T00:00:00Z", End: "2026-01-01T01:00:00Z", Duration: 60}},
		Generated: "gen-1",
	}
	postGuide(t, app, g1)

	g2 := types.Guide{
		Channels: []types.LineupData{
			{StationID: "3", ChannelNumber: "303"},
			{StationID: "4", ChannelNumber: "404"},
			{StationID: "5", ChannelNumber: "505"},
		},
		Programs: []types.Program{
			{Channel: "303", Title: "B", Start: "2026-01-01T00:00:00Z", End: "2026-01-01T01:00:00Z", Duration: 60},
			{Channel: "404", Title: "C", Start: "2026-01-01T00:00:00Z", End: "2026-01-01T01:00:00Z", Duration: 60},
		},
		Generated: "gen-2",
	}
	postGuide(t, app, g2)

	var chCount, progCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM guide_channels").Scan(&chCount); err != nil {
		t.Fatal(err)
	}
	if chCount != 3 {
		t.Errorf("expected 3 guide_channels after replace, got %d", chCount)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM guide_programs").Scan(&progCount); err != nil {
		t.Fatal(err)
	}
	if progCount != 2 {
		t.Errorf("expected 2 guide_programs after replace, got %d", progCount)
	}

	// The old channel must be gone.
	var stale int
	if err := db.QueryRow("SELECT COUNT(*) FROM guide_channels WHERE station_id = '1'").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Errorf("expected old channel gone, found %d", stale)
	}

	// guide_meta should reflect the latest generated value.
	var generated string
	if err := db.QueryRow("SELECT generated FROM guide_meta WHERE id = 1").Scan(&generated); err != nil {
		t.Fatal(err)
	}
	if generated != "gen-2" {
		t.Errorf("expected generated %q, got %q", "gen-2", generated)
	}

	if len(app.guideData.Channels) != 3 || len(app.guideData.Programs) != 2 {
		t.Errorf("expected in-memory 3 channels / 2 programs, got %d / %d", len(app.guideData.Channels), len(app.guideData.Programs))
	}
}

func TestSetGuideMethodNotAllowed(t *testing.T) {
	app, _ := setupTestApp(t)

	r := mux.NewRouter()
	r.HandleFunc("/api/guide", app.setGuide).Methods("POST", "PUT")

	req := httptest.NewRequest("GET", "/api/guide", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("got code %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}

func TestSetGuideBadBody(t *testing.T) {
	app, _ := setupTestApp(t)

	// Invalid JSON body with correct content type.
	req := httptest.NewRequest("POST", "/api/guide", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	app.setGuide(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: got code %d, want %d", rr.Code, http.StatusBadRequest)
	}

	// Wrong content type.
	req2 := httptest.NewRequest("POST", "/api/guide", bytes.NewBufferString(`{"channels":[]}`))
	req2.Header.Set("Content-Type", "text/plain")
	rr2 := httptest.NewRecorder()
	app.setGuide(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Errorf("wrong content type: got code %d, want %d", rr2.Code, http.StatusBadRequest)
	}
}

func TestLoadGuideFromDB(t *testing.T) {
	app, db := setupTestApp(t)
	defer db.Close() //nolint: errcheck

	_, err := db.Exec(`INSERT INTO guide_channels (station_id, channel_number, station_call_sign, logo) VALUES ('1', '101', 'KABC', 'abc.png')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO guide_channels (station_id, channel_number, station_call_sign, logo) VALUES ('2', '202', 'KXTV', 'xtv.png')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO guide_programs (channel, title, subtitle, start, "end", duration, category, is_new) VALUES ('101', 'News', 'Morning', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', 60, 'news', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO guide_programs (channel, title, subtitle, start, "end", duration, category, is_new) VALUES ('202', 'Movie', '', '2026-01-01T02:00:00Z', '2026-01-01T04:00:00Z', 120, 'movie', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO guide_meta (id, generated) VALUES (1, '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	app.loadGuideFromDB()

	if len(app.guideData.Channels) != 2 {
		t.Errorf("expected 2 channels, got %d", len(app.guideData.Channels))
	}
	if len(app.guideData.Programs) != 2 {
		t.Errorf("expected 2 programs, got %d", len(app.guideData.Programs))
	}
	if app.guideData.Generated != "2026-01-01T00:00:00Z" {
		t.Errorf("expected generated %q, got %q", "2026-01-01T00:00:00Z", app.guideData.Generated)
	}

	newsFound := false
	for _, p := range app.guideData.Programs {
		if p.Title == "News" && p.New {
			newsFound = true
		}
	}
	if !newsFound {
		t.Error("expected News program to have New=true decoded from is_new")
	}
}
