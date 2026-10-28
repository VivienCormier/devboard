package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Worktree struct {
	Repo       string    `json:"repo"`
	Branch     string    `json:"branch"`
	Path       string    `json:"path"`
	Dirty      bool      `json:"dirty"`
	Ahead      int64     `json:"ahead"`
	Behind     int64     `json:"behind"`
	Commit     string    `json:"commit,omitempty"`
	CommitAt   time.Time `json:"commit_at,omitempty"`
	DevURL     string    `json:"dev_url,omitempty"`
	IsMain     bool      `json:"is_main"`
	baseBranch string
}

const commandTimeout = 30 * time.Second

func listWorktrees(ctx context.Context, repos []RepoConfig) ([]Worktree, error) {
	var all []Worktree
	var errs []string
	for _, repo := range repos {
		wts, err := listWorktreesWithWt(ctx, repo)
		if err != nil {
			wts, err = listWorktreesWithGit(ctx, repo)
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", repo.Path, err))
			continue
		}
		all = append(all, wts...)
	}
	if len(errs) > 0 {
		return all, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return all, nil
}

// wtEntry is one element of `wt list --format=json` (worktrunk).
type wtEntry struct {
	Branch  *string `json:"branch"`
	Path    string  `json:"path"`
	Kind    string  `json:"kind"`
	IsMain  bool    `json:"is_main"`
	RepoURL string  `json:"repo_url"`
	URL     string  `json:"url"`
	Commit  struct {
		Message   string `json:"message"`
		Timestamp int64  `json:"timestamp"`
	} `json:"commit"`
	WorkingTree struct {
		Staged    bool `json:"staged"`
		Modified  bool `json:"modified"`
		Untracked bool `json:"untracked"`
		Renamed   bool `json:"renamed"`
		Deleted   bool `json:"deleted"`
	} `json:"working_tree"`
	Main struct {
		Ahead  int64 `json:"ahead"`
		Behind int64 `json:"behind"`
	} `json:"main"`
}

func listWorktreesWithWt(ctx context.Context, repo RepoConfig) ([]Worktree, error) {
	out, err := run(ctx, repo.Path, "wt", "list", "--format=json")
	if err != nil {
		return nil, err
	}
	var entries []wtEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parse wt output: %w", err)
	}

	var wts []Worktree
	for _, e := range entries {
		if e.Kind != "" && e.Kind != "worktree" {
			continue
		}
		wt := Worktree{
			Repo:       repoName(e.RepoURL, repo.Path),
			Path:       e.Path,
			Ahead:      e.Main.Ahead,
			Behind:     e.Main.Behind,
			Commit:     e.Commit.Message,
			DevURL:     e.URL,
			IsMain:     e.IsMain,
			baseBranch: repo.BaseBranch,
		}
		if e.Branch != nil {
			wt.Branch = *e.Branch
		}
		if e.Commit.Timestamp > 0 {
			wt.CommitAt = time.Unix(e.Commit.Timestamp, 0)
		}
		w := e.WorkingTree
		// Untracked files alone are too common (scratch files) to count.
		wt.Dirty = w.Staged || w.Modified || w.Renamed || w.Deleted
		wts = append(wts, wt)
	}
	return wts, nil
}

// listWorktreesWithGit is the fallback when worktrunk is missing or fails:
// it knows less (no ahead/behind, no dirty state) but keeps the board usable.
func listWorktreesWithGit(ctx context.Context, repo RepoConfig) ([]Worktree, error) {
	out, err := run(ctx, repo.Path, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	name := repoName("", repo.Path)
	var wts []Worktree
	var cur *Worktree
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, value, _ := strings.Cut(sc.Text(), " ")
		switch key {
		case "worktree":
			wts = append(wts, Worktree{Repo: name, Path: value, IsMain: len(wts) == 0, baseBranch: repo.BaseBranch})
			cur = &wts[len(wts)-1]
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		}
	}
	return wts, sc.Err()
}

// repoName returns "owner/name" from a remote URL, or the folder name.
func repoName(remoteURL, path string) string {
	u := strings.TrimSuffix(remoteURL, ".git")
	if i := strings.Index(u, "github.com"); i >= 0 {
		return strings.Trim(u[i+len("github.com"):], "/:")
	}
	return filepath.Base(path)
}

func run(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
