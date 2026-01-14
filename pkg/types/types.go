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

// Program represents a TV program
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

// First, let's update the Recording struct to include a method for checking status
func (r *Recording) CheckStatus(db *sql.DB, loc *time.Location, storageDir string) string {
	// Construct the expected file path
	var channelName string
	err := db.QueryRow("SELECT guide_name FROM channels WHERE guide_number = ?", r.ChannelID).Scan(&channelName)
	if err != nil {
		return "failed"
	}

	filePath := filepath.Join(storageDir, r.GetFilePath())

	// Check if file exists
	if _, err := os.Stat(filePath); err == nil {
		return "completed"
	} else if !os.IsNotExist(err) {
		// Other error occurred
		return "failed"
	}

	dateTimeStr := fmt.Sprintf("%s %s", r.Date, r.StartTime)
	startTime, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
	if err != nil {
		return "failed"
	}

	// If recording time has passed and no file exists, mark as failed
	if time.Now().In(loc).After(startTime.Add(time.Duration(r.Duration)*time.Minute)) && r.Status == "pending" {
		return "failed"
	}

	// Otherwise, it's still pending
	return "pending"
}
