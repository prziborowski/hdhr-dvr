package types

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	Enabled        *int   `json:"Enabled"`
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
	FileSize  int
}

func (r *Recording) GetFilePath() string {
	var titleStr string
	if r.Title != nil {
		titleStr = *r.Title
	} else {
		titleStr = r.ChannelID
	}
	outputFile := fmt.Sprintf("%s-%s-%s.ts", r.Date, r.StartTime, titleStr)

	return outputFile
}

const (
	PreRollSeconds = 30
	PostRollMinutes = 1
)

func (r *Recording) CheckStatus(db *sql.DB, loc *time.Location, storageDir string) string {
	var channelName string
	err := db.QueryRow("SELECT guide_name FROM channels WHERE guide_number = ?", r.ChannelID).Scan(&channelName)
	if err != nil {
		log.Printf("Error looking up channel %s for recording %d status: %v", r.ChannelID, r.ID, err)
		return "failed"
	}

	filePathTS := filepath.Join(storageDir, r.GetFilePath())
	filePathMP4 := strings.TrimSuffix(filePathTS, filepath.Ext(filePathTS)) + ".mp4"

	fileExists := false
	if _, err := os.Stat(filePathTS); err == nil {
		fileExists = true
	} else if _, err := os.Stat(filePathMP4); err == nil {
		fileExists = true
	}

	dateTimeStr := fmt.Sprintf("%s %s", r.Date, r.StartTime)
	startTime, err := time.ParseInLocation("2006-01-02 15:04", dateTimeStr, loc)
	if err != nil {
		log.Printf("Error parsing start time for recording %d: %v", r.ID, err)
		return "failed"
	}

	endTime := startTime.Add(time.Duration(r.Duration+PostRollMinutes) * time.Minute)
	now := time.Now().In(loc)

	if !fileExists {
		if now.Before(startTime) {
			return "pending"
		} else if now.Before(endTime) {
			return "recording"
		}
		return "failed"
	}

	if now.Before(endTime) {
		return "recording"
	}

	return "completed"
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

// TitanTV API Response Types

type TitanTVChannel struct {
	ChannelID    int    `json:"channelId"`
	MajorChannel int    `json:"majorChannel"`
	MinorChannel int    `json:"minorChannel"`
	CallSign     string `json:"callSign"`
	Description  string `json:"description"`
	Logo         string `json:"logo"`
	ChannelIndex int    `json:"channelIndex"`
}

type TitanTVLineupResponse struct {
	Channels []TitanTVChannel `json:"channels"`
}

type TitanTVEvent struct {
	StartTime   string  `json:"startTime"` // ISO 8601
	EndTime     string  `json:"endTime"`   // ISO 8601
	Title       string  `json:"title"`
	SubTitle    string  `json:"episodeTitle"`
	Description string  `json:"description"`
	ImageURL    string  `json:"imageUrl"`
	SeasonNum   int     `json:"seasonNum"`
	EpisodeNum  int     `json:"episodeNum"`
	Year        int     `json:"year"`
	OriginalAir string  `json:"originalAirDate"`
	Rating      string  `json:"rating"`
	StarRating  float64 `json:"starRating"`
	Genres      string  `json:"genres"`
	ProgramType string  `json:"programType"`
	IsNew       bool    `json:"isNew"`
	NewRepeat   string  `json:"newRepeat"`
}

type TitanTVDay struct {
	Date   string         `json:"date"`
	Events []TitanTVEvent `json:"events"`
}

type TitanTVChannelSchedule struct {
	ChannelIndex int          `json:"channelIndex"`
	Days         []TitanTVDay `json:"days"`
}

type TitanTVScheduleResponse struct {
	Channels []TitanTVChannelSchedule `json:"channels"`
}

type Keyword struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Category  string    `json:"category,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}
