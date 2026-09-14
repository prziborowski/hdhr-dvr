package ffmpeg

import (
	"errors"
	"testing"
)

type fakeRunner struct {
	calls  int
	errs   []error
	argses [][]string
}

func (f *fakeRunner) RunCommand(name string, args ...string) error {
	f.calls++
	f.argses = append(f.argses, append([]string(nil), args...))
	if f.calls-1 < len(f.errs) && f.errs[f.calls-1] != nil {
		return f.errs[f.calls-1]
	}
	return nil
}

func TestConvertToMp4FastPath(t *testing.T) {
	f := &fakeRunner{}

	if err := ConvertToMp4(f, "input.ts", "output.mp4"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if f.calls != 1 {
		t.Fatalf("expected ffmpeg to be called once, got %d", f.calls)
	}

	args := f.argses[0]
	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	if joined != "-i input.ts -c copy -movflags +faststart -y output.mp4 " {
		t.Fatalf("unexpected fast-path args: %q", joined)
	}
}

func TestConvertToMp4Fallback(t *testing.T) {
	fastErr := errors.New("fast conversion failed")
	f := &fakeRunner{errs: []error{fastErr, nil}}

	if err := ConvertToMp4(f, "input.ts", "output.mp4"); err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}

	if f.calls != 2 {
		t.Fatalf("expected ffmpeg to be called twice, got %d", f.calls)
	}

	args := f.argses[1]
	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	if joined != "-err_detect ignore_err -fflags +genpts+discardcorrupt -i input.ts -c copy -map 0 -f matroska -y output.mp4 " {
		t.Fatalf("unexpected fallback args: %q", joined)
	}
}

func TestConvertToMp4BothFail(t *testing.T) {
	fastErr := errors.New("fast conversion failed")
	slowErr := errors.New("slow conversion failed")
	f := &fakeRunner{errs: []error{fastErr, slowErr}}

	err := ConvertToMp4(f, "input.ts", "output.mp4")
	if err == nil {
		t.Fatal("expected error when both conversions fail")
	}
	if !errors.Is(err, slowErr) {
		t.Fatalf("expected wrapped slow error %v, got %v", slowErr, err)
	}

	if f.calls != 2 {
		t.Fatalf("expected ffmpeg to be called twice, got %d", f.calls)
	}
}
