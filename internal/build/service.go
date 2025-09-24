package build

// Package build monitors git history and produces cross-platform binaries.
// We coordinate everything with goroutines, channels, and select statements.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Target describes a GOOS/GOARCH pair we compile for.
type Target struct {
	GOOS   string
	GOARCH string
}

// Result reports the outcome of a single build invocation.
type Result struct {
	Target   Target
	Artifact string
	Err      error
}

// Event bundles all results associated with a triggering commit.
type Event struct {
	Commit  string
	Message string
	Results []Result
	Err     error
}

// Config carries the knobs for the Service so callers can customise it.
type Config struct {
	RepoPath  string
	Binary    string
	OutputDir string
	Targets   []Target
	Interval  time.Duration
}

// DefaultTargets mirrors the Chicha Isotope Map support matrix as a baseline.
func DefaultTargets() []Target {
	return []Target{
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "windows", GOARCH: "amd64"},
	}
}

// Service owns the polling ticker and build execution pipeline.
type Service struct {
	repoPath string
	binary   string
	output   string
	targets  []Target
	interval time.Duration

	events chan Event
	stop   chan struct{}
	done   chan struct{}

	lastCommit string
}

// NewService wires a new Service with sane defaults.
func NewService(cfg Config) *Service {
	if cfg.OutputDir == "" {
		cfg.OutputDir = "dist"
	}
	if cfg.Interval == 0 {
		cfg.Interval = time.Minute
	}
	if len(cfg.Targets) == 0 {
		cfg.Targets = DefaultTargets()
	}
	if cfg.Binary == "" {
		cfg.Binary = "chicha-care"
	}
	return &Service{
		repoPath: cfg.RepoPath,
		binary:   cfg.Binary,
		output:   cfg.OutputDir,
		targets:  append([]Target(nil), cfg.Targets...),
		interval: cfg.Interval,
		events:   make(chan Event, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Events exposes the channel where build events are published.
func (s *Service) Events() <-chan Event {
	return s.events
}

// Start launches the monitoring loop.
func (s *Service) Start() {
	go s.loop()
}

// Stop terminates the monitoring loop and closes the events channel.
func (s *Service) Stop() {
	close(s.stop)
	<-s.done
}

// loop polls git at the configured interval and kicks builds when needed.
func (s *Service) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.runOnce()
	for {
		select {
		case <-ticker.C:
			s.runOnce()
		case <-s.stop:
			close(s.events)
			return
		}
	}
}

// runOnce checks the most recent commit message for the magic phrase.
func (s *Service) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	commit, message, err := s.latestStable(ctx)
	if err != nil {
		s.events <- Event{Err: err}
		return
	}
	if commit == "" || commit == s.lastCommit {
		return
	}
	results := s.buildAll(ctx, commit)
	s.lastCommit = commit
	s.events <- Event{Commit: commit, Message: message, Results: results}
}

// latestStable returns the newest commit containing the Stable Release marker.
func (s *Service) latestStable(ctx context.Context) (string, string, error) {
	if s.repoPath == "" {
		return "", "", errors.New("repository path not configured")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", s.repoPath, "log", "--grep", "Stable Release", "-1", "--pretty=%H%x1f%s")
	out, err := cmd.Output()
	if err != nil {
		// If git exits with status 128 because repo missing, surface the error.
		return "", "", err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "", "", nil
	}
	parts := strings.SplitN(trimmed, "\x1f", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected git output: %q", trimmed)
	}
	return parts[0], parts[1], nil
}

// buildAll iterates over every target and captures the results.
func (s *Service) buildAll(ctx context.Context, commit string) []Result {
	results := make([]Result, 0, len(s.targets))
	for _, target := range s.targets {
		res := s.buildTarget(ctx, commit, target)
		results = append(results, res)
	}
	return results
}

// buildTarget invokes go build with the given GOOS/GOARCH values.
func (s *Service) buildTarget(ctx context.Context, commit string, target Target) Result {
	artifactDir := filepath.Join(s.repoPath, s.output, commit)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return Result{Target: target, Err: err}
	}
	name := fmt.Sprintf("%s-%s-%s", s.binary, target.GOOS, target.GOARCH)
	if target.GOOS == "windows" {
		name += ".exe"
	}
	output := filepath.Join(artifactDir, name)
	args := []string{"build", "-o", output, "./cmd/server"}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = s.repoPath
	cmd.Env = append(os.Environ(), "GOOS="+target.GOOS, "GOARCH="+target.GOARCH)
	if err := cmd.Run(); err != nil {
		return Result{Target: target, Artifact: output, Err: err}
	}
	return Result{Target: target, Artifact: output}
}
