package update

// Package update polls GitHub releases to drive self-updates.
// We rely on channels to notify the rest of the system about new binaries.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config configures the Service with repository and timing details.
type Config struct {
	Owner          string
	Repo           string
	Binary         string
	Interval       time.Duration
	CurrentVersion string
	BaseURL        string
	Client         *http.Client
}

// Event describes an available update payload ready for installation.
type Event struct {
	Version    string
	BinaryPath string
}

// Service periodically queries GitHub for new releases.
type Service struct {
	cfg     Config
	events  chan Event
	stop    chan struct{}
	done    chan struct{}
	client  *http.Client
	lastTag string
}

// NewService builds a Service with defaults aligned to our needs.
func NewService(cfg Config) *Service {
	if cfg.Interval == 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Binary == "" {
		cfg.Binary = "chicha-care"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Service{
		cfg:    cfg,
		events: make(chan Event, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		client: client,
	}
}

// Events exposes the notifications channel.
func (s *Service) Events() <-chan Event {
	return s.events
}

// Start launches the polling loop.
func (s *Service) Start() {
	go s.loop()
}

// Stop stops the polling loop and closes the events channel.
func (s *Service) Stop() {
	close(s.stop)
	<-s.done
}

// loop performs the periodic release lookups.
func (s *Service) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	s.checkOnce()
	for {
		select {
		case <-ticker.C:
			s.checkOnce()
		case <-s.stop:
			close(s.events)
			return
		}
	}
}

// checkOnce fetches the latest release and downloads the asset if new.
func (s *Service) checkOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := s.fetchLatest(ctx)
	if err != nil {
		// We keep the log local to avoid spamming event consumers with noise.
		fmt.Fprintf(os.Stderr, "update check failed: %v\n", err)
		return
	}
	if release.Tag == "" || release.Tag == s.cfg.CurrentVersion || release.Tag == s.lastTag {
		return
	}
	assetURL, assetName := release.AssetFor(runtime.GOOS, runtime.GOARCH, s.cfg.Binary)
	if assetURL == "" {
		fmt.Fprintf(os.Stderr, "update: no asset for %s/%s in release %s\n", runtime.GOOS, runtime.GOARCH, release.Tag)
		return
	}
	path, err := s.download(ctx, assetURL, assetName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update download failed: %v\n", err)
		return
	}
	s.lastTag = release.Tag
	s.events <- Event{Version: release.Tag, BinaryPath: path}
}

// releaseInfo captures the small subset of GitHub release payload we care about.
type releaseInfo struct {
	Tag    string
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// AssetFor returns the asset URL matching the OS/Arch combination.
func (r releaseInfo) AssetFor(goos, goarch, binary string) (string, string) {
	base := fmt.Sprintf("%s-%s-%s", binary, goos, goarch)
	targets := []string{base, base + ".exe"}
	for _, asset := range r.Assets {
		for _, candidate := range targets {
			if strings.EqualFold(asset.Name, candidate) {
				return asset.URL, asset.Name
			}
		}
	}
	return "", ""
}

// fetchLatest queries the GitHub releases API.
func (s *Service) fetchLatest(ctx context.Context) (releaseInfo, error) {
	if s.cfg.Owner == "" || s.cfg.Repo == "" {
		return releaseInfo{}, errors.New("update service requires owner and repo")
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", strings.TrimRight(s.cfg.BaseURL, "/"), s.cfg.Owner, s.cfg.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "chicha-care-updater")
	resp, err := s.client.Do(req)
	if err != nil {
		return releaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return releaseInfo{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
	var payload struct {
		TagName string         `json:"tag_name"`
		Assets  []releaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return releaseInfo{}, err
	}
	assets := make([]struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}, 0, len(payload.Assets))
	for _, a := range payload.Assets {
		assets = append(assets, struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: a.Name, URL: a.URL})
	}
	return releaseInfo{Tag: payload.TagName, Assets: assets}, nil
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// download retrieves the binary and places it near the current executable.
func (s *Service) download(ctx context.Context, url, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "chicha-care-updater")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("download status %d: %s", resp.StatusCode, string(body))
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, name+".part")
	if err != nil {
		return "", err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
