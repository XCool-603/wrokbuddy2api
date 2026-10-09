package server

import (
	"testing"
)

func TestFindMatchingAsset(t *testing.T) {
	assets := []releaseAsset{
		{Name: "workbuddy2api.exe", BrowserDownloadURL: "https://example.com/workbuddy2api.exe", Size: 100},
		{Name: "workbuddy2api-console.exe", BrowserDownloadURL: "https://example.com/workbuddy2api-console.exe", Size: 101},
		{Name: "workbuddy2api-windows-arm64.exe", BrowserDownloadURL: "https://example.com/workbuddy2api-windows-arm64.exe", Size: 102},
		{Name: "workbuddy2api-linux-amd64", BrowserDownloadURL: "https://example.com/workbuddy2api-linux-amd64", Size: 200},
		{Name: "workbuddy2api-linux-arm64", BrowserDownloadURL: "https://example.com/workbuddy2api-linux-arm64", Size: 201},
		{Name: "workbuddy2api-darwin-amd64", BrowserDownloadURL: "https://example.com/workbuddy2api-darwin-amd64", Size: 300},
		{Name: "workbuddy2api-darwin-arm64", BrowserDownloadURL: "https://example.com/workbuddy2api-darwin-arm64", Size: 301},
		{Name: "config.example.json", BrowserDownloadURL: "https://example.com/config.example.json", Size: 10},
	}

	tests := []struct {
		goos    string
		goarch  string
		wantURL string
	}{
		{"windows", "amd64", "https://example.com/workbuddy2api.exe"},
		{"windows", "arm64", "https://example.com/workbuddy2api-windows-arm64.exe"},
		{"linux", "amd64", "https://example.com/workbuddy2api-linux-amd64"},
		{"linux", "arm64", "https://example.com/workbuddy2api-linux-arm64"},
		{"darwin", "amd64", "https://example.com/workbuddy2api-darwin-amd64"},
		{"darwin", "arm64", "https://example.com/workbuddy2api-darwin-arm64"},
		{"unknown", "amd64", ""},
	}

	for _, tt := range tests {
		gotURL, _ := findMatchingAsset(assets, tt.goos, tt.goarch)
		if gotURL != tt.wantURL {
			t.Errorf("findMatchingAsset(%s, %s) = %q, want %q", tt.goos, tt.goarch, gotURL, tt.wantURL)
		}
	}
}
