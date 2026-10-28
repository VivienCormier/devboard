package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is read from a local JSON file that is never committed: it holds the
// paths and names specific to one machine.
type Config struct {
	// Addr is the listen address. Keep it on the loopback interface: the
	// server can open folders on this machine.
	Addr string `json:"addr"`

	// Repos are the git repositories whose worktrees are tracked.
	Repos []RepoConfig `json:"repos"`

	// ClaudeSessionsDir holds the Claude desktop app's session records.
	ClaudeSessionsDir string `json:"claude_sessions_dir"`

	// ClaudeProjectsDir holds the Claude Code transcripts.
	ClaudeProjectsDir string `json:"claude_projects_dir"`

	// SessionMaxAgeDays hides sessions inactive for longer than this.
	SessionMaxAgeDays int64 `json:"session_max_age_days"`

	// EditorCommand opens a worktree in an editor; the path is appended.
	EditorCommand []string `json:"editor_command"`

	// GitHubReviewQuery is the search for pull requests waiting on my
	// review. review-requested also matches requests to my teams; use
	// user-review-requested:@me to keep only those addressed to me.
	GitHubReviewQuery string `json:"github_review_query"`

	// Refresh intervals, in seconds.
	SessionsRefreshSeconds  int64 `json:"sessions_refresh_seconds"`
	WorktreesRefreshSeconds int64 `json:"worktrees_refresh_seconds"`
	GitHubRefreshSeconds    int64 `json:"github_refresh_seconds"`
}

type RepoConfig struct {
	Path string `json:"path"`
	// BaseBranch is the branch work is merged into. Worktrees and sessions
	// on it are not a unit of work of their own.
	BaseBranch string `json:"base_branch"`
}

func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "devboard", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "devboard", "config.json")
}

func loadConfig(path string) (Config, error) {
	home, _ := os.UserHomeDir()
	cfg := Config{
		Addr:                    "127.0.0.1:7777",
		ClaudeSessionsDir:       filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions"),
		ClaudeProjectsDir:       filepath.Join(home, ".claude", "projects"),
		SessionMaxAgeDays:       14,
		EditorCommand:           []string{"code"},
		GitHubReviewQuery:       "is:pr is:open review-requested:@me archived:false",
		SessionsRefreshSeconds:  3,
		WorktreesRefreshSeconds: 30,
		GitHubRefreshSeconds:    90,
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.ClaudeSessionsDir = expandHome(cfg.ClaudeSessionsDir, home)
	cfg.ClaudeProjectsDir = expandHome(cfg.ClaudeProjectsDir, home)
	for i := range cfg.Repos {
		cfg.Repos[i].Path = expandHome(cfg.Repos[i].Path, home)
		if cfg.Repos[i].BaseBranch == "" {
			cfg.Repos[i].BaseBranch = "main"
		}
	}
	return cfg, nil
}

func (c Config) sessionMaxAge() time.Duration {
	return time.Duration(c.SessionMaxAgeDays) * 24 * time.Hour
}

func seconds(n int64) time.Duration {
	return time.Duration(max(n, 1)) * time.Second
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
