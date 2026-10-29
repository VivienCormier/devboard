package main

import (
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
	PermissionMode    string `json:"permissionMode"`
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

	// Approval marks a session that seems stopped on a permission prompt:
	// a tool call that can ask for one has been waiting for its result.
	// The transcript cannot tell it from a long-running command.
	Approval bool `json:"approval,omitempty"`

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

	// Known from GitHub only; a PR known from a session record has none.
	Title             string `json:"title,omitempty"`
	Author            string `json:"author,omitempty"`
	Draft             bool   `json:"draft,omitempty"`
	ReviewDecision    string `json:"review_decision,omitempty"`
	Mergeable         string `json:"mergeable,omitempty"`
	Checks            string `json:"checks,omitempty"`
	UnresolvedThreads int64  `json:"unresolved_threads,omitempty"`
	Mine              bool   `json:"mine,omitempty"`
	ReviewRequested   bool   `json:"review_requested,omitempty"`

	// own is the session record's flag for a PR its user authored.
	own bool
}

// workingWindow bounds how long a turn with no news is still shown as
// running, so a crashed session does not stay "working" forever.
const workingWindow = 15 * time.Minute

// approvalAfter is how long a tool call that can ask for a permission must
// have been waiting, with nothing written to the transcript since, before
// the session is shown as probably waiting for an approval.
const approvalAfter = 20 * time.Second

func (d desktopSession) toSession(now time.Time, t transcript) Session {
	s := Session{
		ID:        d.SessionID,
		Title:     d.Title,
		Cwd:       d.Cwd,
		State:     StateIdle,
		Activity:  time.UnixMilli(d.LastActivityAt),
		URL:       "claude://code/continue?session=" + d.SessionID,
		gitBranch: t.branch,
	}
	if s.Title == "" {
		s.Title = filepath.Base(d.Cwd)
	}

	// lastTurnEdgeAt moves at both ends of a turn; a user message at or
	// after it means the turn it started has not ended yet. The summary and
	// wrap-up are cleared while a turn runs.
	turnOpen := d.PostTurnSummary == nil && d.TurnWrapUp == nil &&
		d.LatestUserFrameAt >= d.LastTurnEdgeAt-2000
	running := turnOpen && now.Sub(s.Activity) < workingWindow

	// A question or a permission prompt stops a turn without ending it, so
	// the session record still describes a running turn: only the
	// transcript shows the tool call waiting on the person. The working
	// window does not apply, since they can wait for hours.
	switch {
	case turnOpen && t.pending.isQuestion():
		s.State = StateWaiting
		s.Detail = t.pending.detail
	case turnOpen && t.pending.mayAskApproval(d.PermissionMode) && now.Sub(t.mtime) >= approvalAfter:
		s.State = StateWaiting
		s.Approval = true
		s.Detail = t.pending.name
		if t.pending.detail != "" {
			s.Detail += ": " + t.pending.detail
		}
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
		s.prs = append(s.prs, PR{Number: pr.Number, Repo: pr.Repo, URL: pr.URL, State: pr.State, Branch: pr.Branch, own: pr.Own})
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
	transcripts map[string]transcript
}

type cachedSession struct {
	mtime   time.Time
	session desktopSession
}

func newSessionScanner(dir, projectsDir string, maxAge time.Duration) *sessionScanner {
	return &sessionScanner{
		dir:         dir,
		projectsDir: projectsDir,
		maxAge:      maxAge,
		cache:       map[string]cachedSession{},
		transcripts: map[string]transcript{},
	}
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
		t := sc.transcript(d)
		s := d.toSession(now, t)
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
