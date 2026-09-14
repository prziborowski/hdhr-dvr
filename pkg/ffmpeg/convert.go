package ffmpeg

import (
	"fmt"
	"log"
)

// Runner runs an external command by name. Both *RealCommander and *MockCommander
// in cmd/app satisfy this interface via their RunCommand method.
type Runner interface {
	RunCommand(name string, args ...string) error
}

// ConvertToMp4 transcodes a .ts file to .mp4. It first attempts a fast copy
// and, on failure, falls back to a slower, more tolerant conversion.
func ConvertToMp4(r Runner, tsFile, mp4File string) error {
	log.Printf("Converting %s to %s...", tsFile, mp4File)
	args := []string{
		"-i", tsFile,
		"-c", "copy",
		"-movflags", "+faststart",
		"-y",
		mp4File,
	}
	err := r.RunCommand("ffmpeg", args...)
	if err != nil {
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
		if err = r.RunCommand("ffmpeg", args...); err != nil {
			return fmt.Errorf("ffmpeg conversion failed: %w", err)
		}
	}
	return nil
}
