package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ValidAllFields(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configContent := `{
		"timezone": "America/New_York",
		"lineUpID": "test-lineup",
		"days": 5,
		"guideFile": "epg.json",
		"stateFile": "state.json",
		"storageDir": "/tmp/recordings",
		"userId": "test-user-id"
	}`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertString(t, "timezone", cfg.Timezone, "America/New_York")
	assertString(t, "userID", cfg.UserID, "test-user-id")
	assertString(t, "lineUpID", cfg.LineUpID, "test-lineup")
	assertInt(t, "days", cfg.Days, 5)
	assertString(t, "guideFile", cfg.GuideFile, "epg.json")
	assertString(t, "stateFile", cfg.StateFile, "state.json")
	assertString(t, "storageDir", cfg.StorageDir, "/tmp/recordings")
}

func TestLoadConfig_DefaultTimezone(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configContent := `{
		"lineUpID": "test",
		"storageDir": "/tmp/rec"
	}`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertString(t, "timezone default", cfg.Timezone, "America/Los_Angeles")
}

func TestLoadConfig_DaysClampedTo8_Zero(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configContent := `{
		"lineUpID": "test",
		"days": 0,
		"storageDir": "/tmp/rec"
	}`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertInt(t, "days clamped to 8", cfg.Days, 8)
}

func TestLoadConfig_DaysClampedTo8_GreaterThan8(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configContent := `{
		"lineUpID": "test",
		"days": 14,
		"storageDir": "/tmp/rec"
	}`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertInt(t, "days clamped to 8", cfg.Days, 8)
}

func TestLoadConfig_DefaultGuideFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configContent := `{
		"lineUpID": "test",
		"storageDir": "/tmp/rec"
	}`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertString(t, "guideFile default", cfg.GuideFile, "guide.json")
}

func TestLoadConfig_DefaultStateFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configContent := `{
		"lineUpID": "test",
		"storageDir": "/tmp/rec"
	}`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertString(t, "stateFile default", cfg.StateFile, "guide_state.json")
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte("not valid json {{{"), 0644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(wd)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing config.json")
	}
}

func assertString(t *testing.T, label, got, want string) {
	if got != want {
		t.Fatalf("%s: expected %q, got %q", label, want, got)
	}
}

func assertInt(t *testing.T, label string, got, want int) {
	if got != want {
		t.Fatalf("%s: expected %d, got %d", label, want, got)
	}
}
