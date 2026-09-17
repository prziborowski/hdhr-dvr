package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prziborowski/hdhr-dvr/pkg/types"
)

func TestFetchLocalChannels_HonorsBaseURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		channels := []types.Channel{
			{GuideNumber: "3", GuideName: "ABC"},
			{GuideNumber: "7", GuideName: "NBC"},
		}
		if err := json.NewEncoder(w).Encode(channels); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	channels, err := fetchLocalChannels(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(channels))
	}
	if gotPath != "/api/channels" {
		t.Fatalf("expected request path %q, got %q", "/api/channels", gotPath)
	}
}
