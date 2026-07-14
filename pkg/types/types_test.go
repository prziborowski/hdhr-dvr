package types

import (
	"testing"
)

func TestGetFilePath_WithTitle(t *testing.T) {
	title := "Test Show"
	r := Recording{
		ChannelID: "001",
		Date:      "2026-01-01",
		StartTime: "19:30",
		Title:     &title,
	}
	want := "2026-01-01-19:30-Test Show.ts"
	got := r.GetFilePath()
	if got != want {
		t.Fatalf("GetFilePath: expected %q, got %q", want, got)
	}
}

func TestGetFilePath_WithoutTitle(t *testing.T) {
	r := Recording{
		ChannelID: "042",
		Date:      "2026-05-15",
		StartTime: "08:00",
		Title:     nil,
	}
	want := "2026-05-15-08:00-042.ts"
	got := r.GetFilePath()
	if got != want {
		t.Fatalf("GetFilePath: expected %q, got %q", want, got)
	}
}

func TestGetFilePath_TitleWithSpecialChars(t *testing.T) {
	title := "Test Show & More (2026)"
	r := Recording{
		ChannelID: "007",
		Date:      "2026-03-20",
		StartTime: "21:00",
		Title:     &title,
	}
	want := "2026-03-20-21:00-Test Show & More (2026).ts"
	got := r.GetFilePath()
	if got != want {
		t.Fatalf("GetFilePath: expected %q, got %q", want, got)
	}
}

func TestGetFilePath_EmptyTitle(t *testing.T) {
	title := ""
	r := Recording{
		ChannelID: "012",
		Date:      "2026-07-04",
		StartTime: "12:00",
		Title:     &title,
	}
	want := "2026-07-04-12:00-.ts"
	got := r.GetFilePath()
	if got != want {
		t.Fatalf("GetFilePath: expected %q, got %q", want, got)
	}
}

func TestProgramChannelField(t *testing.T) {
	p := Program{Channel: "001"}
	if p.Channel != "001" {
		t.Fatalf("expected Channel to be '001', got %q", p.Channel)
	}
}

func TestProgramNewFlag(t *testing.T) {
	p := Program{New: true, Title: "New Show"}
	if !p.New {
		t.Fatal("expected New to be true")
	}
}

func TestChannelEnabledField(t *testing.T) {
	c := Channel{GuideNumber: "001", Enabled: nil}
	if c.Enabled != nil {
		t.Fatal("expected Enabled to be nil")
	}
}

func TestLineupDataFields(t *testing.T) {
	l := LineupData{StationID: "s1", ChannelNumber: "12"}
	if l.StationID != "s1" || l.ChannelNumber != "12" {
		t.Fatal("expected StationID and ChannelNumber to match")
	}
}
