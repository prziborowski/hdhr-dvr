package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	pkgcfg "github.com/prziborowski/hdhr-dvr/pkg/config"
	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

func main() {
	// Load config to get storage directory
	config, err := pkgcfg.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Open database
	db, err := sql.Open("sqlite3", "./recordings.db")
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close() // nolint: errcheck

	// Get all recordings
	rows, err := db.Query("SELECT id, channel_id, date, start_time, duration, title FROM recordings")
	if err != nil {
		log.Fatalf("Failed to query recordings: %v", err)
	}

	var records []struct {
		r     types.Recording
		title sql.NullString
	}

	for rows.Next() {
		var r types.Recording
		var title sql.NullString
		err := rows.Scan(&r.ID, &r.ChannelID, &r.Date, &r.StartTime, &r.Duration, &title)
		if err != nil {
			log.Printf("Error scanning recording: %v", err)
			continue
		}
		records = append(records, struct {
			r     types.Recording
			title sql.NullString
		}{r, title})
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating recordings: %v", err)
	}
	if err := rows.Close(); err != nil { // nolint: errcheck
		log.Printf("Error closing rows cursor: %v", err)
	} // Close rows before performing updates to avoid database lock

	count := 0
	updated := 0

	for _, rec := range records {
		r := rec.r
		if rec.title.Valid {
			r.Title = &rec.title.String
		}

		count++

		// Determine the file path
		tsFile := filepath.Join(config.StorageDir, r.GetFilePath())
		mp4File := strings.TrimSuffix(tsFile, filepath.Ext(tsFile)) + ".mp4"

		var finalSize int64 = 0
		found := false

		// Prefer MP4 if it exists
		if info, err := os.Stat(mp4File); err == nil {
			finalSize = info.Size()
			found = true
		} else if info, err := os.Stat(tsFile); err == nil {
			// Fallback to TS file
			finalSize = info.Size()
			found = true
		}

		if found {
			_, err := db.Exec("UPDATE recordings SET file_size = ? WHERE id = ?", finalSize, r.ID)
			if err != nil {
				log.Printf("Failed to update size for recording %d: %v", r.ID, err)
			} else {
				updated++
			}
		} else {
			log.Printf("File not found for recording %d (%s)", r.ID, r.GetFilePath())
		}
	}

	fmt.Printf("Processed %d recordings, updated %d file sizes.\n", count, updated)
}
