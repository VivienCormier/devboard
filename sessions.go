package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// desktopSession is the subset of a Claude desktop session record
// (claude-code-sessions/<account>/<org>/local_<id>.json) that devboard reads.
// The format is the app's internal one and may change without notice.
type desktopSession struct {
	SessionID         string `json:"sessionId"`
	CLISessionID      string `json:"cliSessionId"`
	Cwd               string `json:"cwd"`
	Title             string `json:"title"`
	IsArchived        bool   `json:"isArchived"`
	LastActivityAt    int64  `json:"lastActivityAt"`
	LastTurnEdgeAt    int64  `json:"lastTurnEdgeAt"`
	LatestUserFrameAt int64  `json:"latestUserFrameAt"`
	PRs               []struct {
		Number int64  `json:"prNumber"`
		Repo   string `json:"repo"`
		URL    string `json:"url"`
		Branch string `json:"branch"`
		State  string `json:"state"`
		Own    bool   `json:"own"`
	} `json:"prs"`
	// WrittenBranches entries are "<cwd>\x00<branch>", oldest first.
	WrittenBranches []string `json:"writtenBranches"`
	PostTurnSummary *struct {
		StatusCategory string `json:"status_category"`
		StatusDetail   string `json:"status_detail"`
		NeedsAction    string `json:"needs_action"`
	} `json:"postTurnSummary"`
	TurnWrapUp *struct {
		Description string `json:"description"`
		Ask         string `json:"ask"`
	} `json:"turnWrapUp"`
	InterruptedAwaitingPerson json.RawMessage `json:"interruptedAwaitingPerson"`
}

// Session states, from the most to the least urgent.
const (
	StateWaiting = "waiting" // Claude asked something and waits for an answer
	StateReview  = "review"  // the work is ready to be reviewed
	StateWorking = "working" // a turn is running
	StateIdle    = "idle"
)

type Session struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Cwd      string    `json:"cwd"`
	State    string    `json:"state"`
	Detail   string    `json:"detail,omitempty"`
	Activity time.Time `json:"activity"`
	URL      string    `json:"url"`

	// gitBranch is the branch checked out in cwd at the last message, read
	// from the CLI transcript.
	gitBranch string
	branches  []string
	prs       []PR
}

type PR struct {
	Number int64  `json:"number"`
	Repo   string `json:"repo"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Branch string `json:"-"`
}

// workingWindow bounds how long a turn with no news is still shown as
// running, so a crashed session does not stay "working" forever.
const workingWindow = 15 * time.Minute

func (d desktopSession) toSession(now time.Time) Session {
	s := Session{
		ID:       d.SessionID,
		Title:    d.Title,
		Cwd:      d.Cwd,
		State:    StateIdle,
		Activity: time.UnixMilli(d.LastActivityAt),
		URL:      "claude://code/continue?session=" + d.SessionID,
	}
	if s.Title == "" {
		s.Title = filepath.Base(d.Cwd)
	}

	// lastTurnEdgeAt moves at both ends of a turn; a user message at or
	// after it means the turn it started has not ended yet. The summary and
	// wrap-up are cleared while a turn runs.
	running := d.PostTurnSummary == nil && d.TurnWrapUp == nil &&
		d.LatestUserFrameAt >= d.LastTurnEdgeAt-2000 &&
		now.Sub(s.Activity) < workingWindow

	switch {
	case running:
		s.State = StateWorking
	case d.TurnWrapUp != nil && d.TurnWrapUp.Ask != "":
		s.State = StateWaiting
		s.Detail = d.TurnWrapUp.Ask
	case d.PostTurnSummary != nil && d.PostTurnSummary.StatusCategory == "blocked":
		s.State = StateWaiting
		s.Detail = d.PostTurnSummary.NeedsAction
	case len(d.InterruptedAwaitingPerson) > 0 && string(d.InterruptedAwaitingPerson) != "null":
		s.State = StateWaiting
		s.Detail = "Interrupted, waiting for you"
	case d.PostTurnSummary != nil && d.PostTurnSummary.StatusCategory == "review_ready":
		s.State = StateReview
		s.Detail = d.PostTurnSummary.StatusDetail
	}
	if s.Detail == "" && d.PostTurnSummary != nil {
		s.Detail = d.PostTurnSummary.StatusDetail
	}
	if s.Detail == "" && d.TurnWrapUp != nil {
		s.Detail = d.TurnWrapUp.Description
	}

	for _, pr := range d.PRs {
		s.prs = append(s.prs, PR{Number: pr.Number, Repo: pr.Repo, URL: pr.URL, State: pr.State, Branch: pr.Branch})
	}
	for _, wb := range d.WrittenBranches {
		if _, branch, ok := strings.Cut(wb, "\x00"); ok && branch != "" {
			s.branches = append(s.branches, branch)
		}
	}
	return s
}

// sessionScanner rereads only the session records whose mtime changed: the
// records embed tool schemas and weigh tens of kilobytes each.
type sessionScanner struct {
	dir         string
	projectsDir string
	maxAge      time.Duration
	cache       map[string]cachedSession
	transcripts map[string]cachedBranch
}

type cachedSession struct {
	mtime   time.Time
	session desktopSession
}

type cachedBranch struct {
	path   string
	mtime  time.Time
	branch string
}

func newSessionScanner(dir, projectsDir string, maxAge time.Duration) *sessionScanner {
	return &sessionScanner{
		dir:         dir,
		projectsDir: projectsDir,
		maxAge:      maxAge,
		cache:       map[string]cachedSession{},
		transcripts: map[string]cachedBranch{},
	}
}

// transcriptTail bounds how much of a transcript is read to find its last
// gitBranch: every message carries it, so the end of the file is enough.
const transcriptTail = 256 << 10

var gitBranchKey = []byte(`"gitBranch":"`)

// gitBranch returns the branch recorded on the last message of the session's
// CLI transcript (~/.claude/projects/<encoded cwd>/<cli session id>.jsonl).
func (sc *sessionScanner) gitBranch(d desktopSession) string {
	if d.CLISessionID == "" {
		return ""
	}
	cached := sc.transcripts[d.CLISessionID]
	path := cached.path
	if path == "" {
		path = filepath.Join(sc.projectsDir, encodeProjectDir(d.Cwd), d.CLISessionID+".jsonl")
		if _, err := os.Stat(path); err != nil {
			matches, _ := filepath.Glob(filepath.Join(sc.projectsDir, "*", d.CLISessionID+".jsonl"))
			if len(matches) == 0 {
				return ""
			}
			path = matches[0]
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		delete(sc.transcripts, d.CLISessionID)
		return ""
	}
	if cached.path == path && cached.mtime.Equal(info.ModTime()) {
		return cached.branch
	}

	branch := ""
	if f, err := os.Open(path); err == nil {
		offset := max(info.Size()-transcriptTail, 0)
		buf := make([]byte, info.Size()-offset)
		if n, _ := f.ReadAt(buf, offset); n > 0 {
			buf = buf[:n]
			if i := bytes.LastIndex(buf, gitBranchKey); i >= 0 {
				rest := buf[i+len(gitBranchKey):]
				if j := bytes.IndexByte(rest, '"'); j >= 0 {
					branch = string(rest[:j])
				}
			}
		}
		f.Close()
	}
	sc.transcripts[d.CLISessionID] = cachedBranch{path: path, mtime: info.ModTime(), branch: branch}
	return branch
}

// encodeProjectDir mirrors how Claude Code names a project's transcript
// folder: every character other than a letter, digit or dash becomes a dash.
func encodeProjectDir(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, cwd)
}

func (sc *sessionScanner) scan(now time.Time) []Session {
	seen := map[string]bool{}
	var sessions []Session

	err := filepath.WalkDir(sc.dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		// A record untouched for longer than maxAge cannot be recent.
		if now.Sub(info.ModTime()) > sc.maxAge {
			return nil
		}
		seen[path] = true

		cached, ok := sc.cache[path]
		if !ok || !cached.mtime.Equal(info.ModTime()) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			var d desktopSession
			if err := json.Unmarshal(data, &d); err != nil {
				// The app rewrites the file in place; retry on the next scan.
				return nil
			}
			cached = cachedSession{mtime: info.ModTime(), session: d}
			sc.cache[path] = cached
		}

		d := cached.session
		if d.IsArchived || now.Sub(time.UnixMilli(d.LastActivityAt)) > sc.maxAge {
			return nil
		}
		s := d.toSession(now)
		s.gitBranch = sc.gitBranch(d)
		sessions = append(sessions, s)
		return nil
	})
	if err != nil {
		log.Printf("scan sessions: %v", err)
	}

	for path, cached := range sc.cache {
		if !seen[path] {
			delete(sc.cache, path)
			delete(sc.transcripts, cached.session.CLISessionID)
		}
	}
	return sessions
}
