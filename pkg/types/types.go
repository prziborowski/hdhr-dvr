package types

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
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

type Program struct {
	Channel  string `json:"channel"`
	Title    string `json:"title"`
	SubTitle string `json:"subtitle"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Duration int    `json:"duration"`
	Category string `json:"category,omitempty"`
	Video    struct {
		Quality string `json:"quality,omitempty"`
	} `json:"video,omitempty"`
	Audio struct {
		Stereo string `json:"stereo,omitempty"`
	} `json:"audio,omitempty"`
	New bool `json:"new,omitempty"`
}

type Recording struct {
	ID        int
	ChannelID string
	Date      string // YYYY-MM-DD
	StartTime string // HH:MM
	Duration  int    // Duration in minutes
	Status    string
	Title     *string `json:"title,omitempty"`
	CreatedAt time.Time
}

func (r *Recording) GetFilePath() string {
	var titleStr string
	if r.Title != nil {
		titleStr = *r.Title
	} else {
		titleStr = r.ChannelID
	}
	outputFile := fmt.Sprintf("%s-%s-%s.mp4", r.Date, r.StartTime, titleStr)

	return outputFile
}

func (r *Recording) CheckStatus(db *sql.DB, loc *time.Location, storageDir string) string {
	// Construct the expected file path
	var channelName string
	err := db.QueryRow("SELECT guide_name FROM channels WHERE guide_number = ?", r.ChannelID).Scan(&channelName)
	if err != nil {
		return "failed"
	}

	filePath := filepath.Join(storageDir, r.GetFilePath())

	// Check if file exists and is valid
	if _, err := os.Stat(filePath); err == nil {
		return "completed"
	}

	// Calculate end time (with pre-roll)
	dateTimeStr := fmt.Sprintf("%s %s", r.Date, r.StartTime)
	startTime, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
	if err != nil {
		return "failed"
	}

	endTime := startTime.Add(time.Duration(r.Duration+1) * time.Minute) // +1 for pre-roll
	now := time.Now().In(loc)

	if now.Before(startTime) {
		return "pending"
	} else if now.Before(endTime) {
		return "recording"
	}

	return "failed"
}

type Guide struct {
	Channels  []LineupData `json:"channels"`
	Programs  []Program    `json:"programs"`
	Generated string       `json:"generated"`
}

type LineupData struct {
	StationID       string `json:"stationId"`
	ChannelNumber   string `json:"channelNumber"`
	StationCallSign string `json:"stationCallSign"`
	Logo            string `json:"logo"`
}

type ListingData struct {
	ProgramID string   `json:"programId"`
	Title     string   `json:"title"`
	Subtitle  string   `json:"subtitle"`
	Flags     []string `json:"flags"`
	Type      string   `json:"type"`
	StartTime string   `json:"startTime"`
	Start     int      `json:"start"`
	Duration  int      `json:"duration"`
	RunTime   int      `json:"runTime"`
}

type Keyword struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Category  string    `json:"category,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}
