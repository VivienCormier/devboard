package main

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Unit is one piece of work, keyed by its branch: the worktree it is checked
// out in, the pull request it became, and every Claude session working on it.
type Unit struct {
	Branch    string    `json:"branch"`
	Repo      string    `json:"repo,omitempty"`
	Worktree  *Worktree `json:"worktree,omitempty"`
	PR        *PR       `json:"pr,omitempty"`
	Tickets   []Ticket  `json:"tickets,omitempty"`
	Sessions  []Session `json:"sessions"`
	Attention []string  `json:"attention"`
	Working   bool      `json:"working"`
	Activity  time.Time `json:"activity"`
}

type Board struct {
	Units []Unit `json:"units"`
	// Loose are sessions tied to no live unit: other projects, sessions on
	// the base branch, sessions on work that already landed.
	Loose []Session `json:"loose"`
	// Tickets are my open tickets tied to no unit: not started yet, or
	// worked on without a branch that names them.
	Tickets []Ticket `json:"tickets"`
	// GitHub and Notion are false until they could be fetched once.
	GitHub bool `json:"github"`
	Notion bool `json:"notion"`
	// NotionConnect asks for a Notion sign-in: OAuth is set up and there
	// is no usable token.
	NotionConnect bool      `json:"notion_connect,omitempty"`
	Errors        []string  `json:"errors,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// PR state for a pull request a session record still sees open but GitHub no
// longer lists among my open ones: merged or closed, without saying which.
const prStateDone = "DONE"

func buildBoard(sessions []Session, worktrees []Worktree, gh *GitHub, tickets []Ticket, errs []string, now time.Time) Board {
	units := map[string]*Unit{}
	unit := func(branch string) *Unit {
		u, ok := units[branch]
		if !ok {
			u = &Unit{Branch: branch}
			units[branch] = u
		}
		return u
	}

	baseBranches := map[string]bool{}
	for _, wt := range worktrees {
		baseBranches[wt.baseBranch] = true
	}

	for i := range worktrees {
		wt := worktrees[i]
		if wt.Branch == "" || baseBranches[wt.Branch] {
			continue
		}
		u := unit(wt.Branch)
		u.Worktree = &wt
		u.Repo = wt.Repo
	}

	ghOpen := map[string]PR{}
	ghDone := map[string]PR{}
	if gh != nil {
		for _, pr := range gh.Open {
			ghOpen[pr.URL] = pr
			u := unit(pr.Branch)
			u.PR = &pr
			u.Repo = pr.Repo
		}
		for _, pr := range gh.Done {
			ghDone[pr.URL] = pr
		}
	}

	var loose []Session
	for _, s := range sessions {
		// A session record keeps the state its PRs had when the session
		// last looked; GitHub is fresher.
		if gh != nil {
			// The scan's sessions are shared between builds.
			s.prs = slices.Clone(s.prs)
			for i, pr := range s.prs {
				switch {
				case ghOpen[pr.URL].URL != "":
					s.prs[i].State = "OPEN"
				case ghDone[pr.URL].URL != "":
					s.prs[i].State = ghDone[pr.URL].State
				case pr.own && pr.State == "OPEN":
					s.prs[i].State = prStateDone
				}
			}
		}

		branches := sessionBranches(s, worktrees, baseBranches)
		if len(branches) == 0 {
			loose = append(loose, s)
			continue
		}
		for _, b := range branches {
			u := unit(b)
			u.Sessions = append(u.Sessions, s)
		}
		for _, pr := range s.prs {
			u, ok := units[pr.Branch]
			if !ok || (u.PR != nil && (u.PR.Title != "" || u.PR.State == "OPEN")) {
				continue
			}
			pr := pr
			u.PR = &pr
			if u.Repo == "" {
				u.Repo = pr.Repo
			}
		}
	}

	// A worktree whose PR is no longer open: say it landed rather than
	// flag it as work without a PR.
	if gh != nil {
		for _, pr := range gh.Done {
			if u, ok := units[pr.Branch]; ok && u.PR == nil {
				pr := pr
				u.PR = &pr
			}
		}
	}

	board := Board{Loose: loose, GitHub: gh != nil, Notion: tickets != nil, Errors: errs, UpdatedAt: now}

	// A ticket joins every unit whose branch or PR title names it, or whose
	// PR its page links to. The PRs it links to get what GitHub knows.
	tickets = slices.Clone(tickets)
	matched := make([]bool, len(tickets))
	for i := range tickets {
		t := &tickets[i]
		t.PRs = slices.Clone(t.PRs)
		for j, pr := range t.PRs {
			if known, ok := ghOpen[pr.URL]; ok {
				t.PRs[j].State, t.PRs[j].Title = "OPEN", known.Title
			} else if known, ok := ghDone[pr.URL]; ok {
				t.PRs[j].State, t.PRs[j].Title = known.State, known.Title
			}
		}
		for _, u := range units {
			if ticketMatches(*t, u) {
				u.Tickets = append(u.Tickets, *t)
				matched[i] = true
			}
		}
	}
	// A ticket with several PRs is still being worked on while one of them
	// is open, even if another one merged.
	stillOpen := map[string]bool{}
	for _, t := range tickets {
		if slices.ContainsFunc(t.PRs, func(pr PR) bool { return pr.State == "OPEN" }) {
			stillOpen[t.URL] = true
		}
	}
	for _, u := range units {
		if u.PR != nil && u.PR.State == "OPEN" {
			for _, t := range u.Tickets {
				stillOpen[t.URL] = true
			}
		}
	}
	for i, t := range tickets {
		if matched[i] {
			continue
		}
		if t.Group == TicketDoing && landed(t.PRs) {
			t.Attention = append(t.Attention, "PR merged, ticket still "+t.Status)
		}
		board.Tickets = append(board.Tickets, t)
	}

	for _, u := range units {
		u.Sessions = sortSessions(u.Sessions)
		open := u.PR != nil && u.PR.State == "OPEN"
		// A branch with no worktree and no open PR is gone or done: its idle
		// sessions are only history.
		// A ticket still in progress keeps it, to say the ticket has to move.
		if u.Worktree == nil && !open && !slices.ContainsFunc(u.Tickets, inProgress) &&
			(len(u.Sessions) == 0 || u.Sessions[0].State == StateIdle) {
			board.Loose = append(board.Loose, u.Sessions...)
			continue
		}

		if u.Worktree != nil {
			u.Activity = u.Worktree.CommitAt
		}
		for _, s := range u.Sessions {
			switch s.State {
			case StateWaiting:
				if s.Approval {
					u.Attention = append(u.Attention, "Claude may need a tool approval")
				} else {
					u.Attention = append(u.Attention, "Claude is waiting for you")
				}
			case StateReview:
				u.Attention = append(u.Attention, "Session ready for review")
			case StateWorking:
				u.Working = true
			}
			if s.Activity.After(u.Activity) {
				u.Activity = s.Activity
			}
		}
		u.Attention = slices.Compact(u.Attention)

		if open && u.PR.Title != "" {
			u.Attention = append(u.Attention, prAttention(*u.PR)...)
		}
		if u.PR != nil && (u.PR.State == "MERGED" || u.PR.State == prStateDone) {
			for _, t := range u.Tickets {
				if inProgress(t) && !stillOpen[t.URL] {
					u.Attention = append(u.Attention, cmp.Or(t.ID, "Ticket")+" still "+t.Status)
				}
			}
		}
		if gh != nil && u.PR == nil && u.Worktree != nil &&
			(u.Worktree.Dirty || u.Worktree.Ahead > 0) && now.Sub(u.Activity) > staleWorkAfter {
			u.Attention = append(u.Attention, "No PR yet")
		}
		board.Units = append(board.Units, *u)
	}
	board.Loose = sortSessions(board.Loose)

	slices.SortFunc(board.Units, func(a, b Unit) int {
		if c := cmp.Compare(unitRank(a), unitRank(b)); c != 0 {
			return c
		}
		return b.Activity.Compare(a.Activity)
	})
	return board
}

func ticketMatches(t Ticket, u *Unit) bool {
	if t.ID != "" && (mentionsTicket(u.Branch, t.ID) || u.PR != nil && mentionsTicket(u.PR.Title, t.ID)) {
		return true
	}
	return u.PR != nil && slices.ContainsFunc(t.PRs, func(pr PR) bool { return pr.URL == u.PR.URL })
}

func inProgress(t Ticket) bool { return t.Group == TicketDoing }

// landed reports whether a ticket's PRs are all known, closed, and at least
// one of them merged.
func landed(prs []PR) bool {
	merged := false
	for _, pr := range prs {
		switch pr.State {
		case "MERGED":
			merged = true
		case "CLOSED":
		default:
			return false
		}
	}
	return merged
}

func unitRank(u Unit) int {
	switch {
	case len(u.Attention) > 0:
		return 0
	case u.Working:
		return 1
	default:
		return 2
	}
}

// sessionBranches decides which units a session belongs to: the branches of
// its open pull requests, else the branch of its last message, else the last
// branch it wrote to, else the branch of the linked worktree it runs in.
func sessionBranches(s Session, worktrees []Worktree, base map[string]bool) []string {
	var branches []string
	for _, pr := range s.prs {
		if pr.State == "OPEN" && pr.Branch != "" && !base[pr.Branch] {
			branches = append(branches, pr.Branch)
		}
	}
	if len(branches) > 0 {
		slices.Sort(branches)
		return slices.Compact(branches)
	}

	// The branch checked out where the session started is its work only if
	// it changed files there. A session that edited files elsewhere only
	// (another repository, another worktree) belongs to the worktrees it
	// edited, or to no branch.
	if len(s.edited) > 0 {
		root := s.Cwd
		if wt := worktreeFor(s.Cwd, worktrees); wt != nil {
			root = wt.Path
		}
		if !slices.ContainsFunc(s.edited, func(path string) bool { return within(path, root) }) {
			for _, path := range s.edited {
				if wt := worktreeFor(path, worktrees); wt != nil && !wt.IsMain && wt.Branch != "" && !base[wt.Branch] {
					branches = append(branches, wt.Branch)
				}
			}
			slices.Sort(branches)
			return slices.Compact(branches)
		}
	}

	if s.gitBranch != "" && s.gitBranch != "HEAD" && !base[s.gitBranch] {
		return []string{s.gitBranch}
	}
	for i := len(s.branches) - 1; i >= 0; i-- {
		if !base[s.branches[i]] {
			return []string{s.branches[i]}
		}
	}
	// The main checkout switches branches over time: what it has checked out
	// now says nothing about an older session that ran there.
	if wt := worktreeFor(s.Cwd, worktrees); wt != nil && !wt.IsMain && wt.Branch != "" && !base[wt.Branch] {
		return []string{wt.Branch}
	}
	return nil
}

// worktreeFor returns the worktree with the longest path containing dir:
// linked worktrees can live inside the main one (.claude/worktrees/...).
func worktreeFor(dir string, worktrees []Worktree) *Worktree {
	var best *Worktree
	for i := range worktrees {
		p := worktrees[i].Path
		if within(dir, p) {
			if best == nil || len(p) > len(best.Path) {
				best = &worktrees[i]
			}
		}
	}
	return best
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func sortSessions(sessions []Session) []Session {
	rank := map[string]int{StateWaiting: 0, StateReview: 1, StateWorking: 2, StateIdle: 3}
	slices.SortFunc(sessions, func(a, b Session) int {
		if c := cmp.Compare(rank[a.State], rank[b.State]); c != 0 {
			return c
		}
		return b.Activity.Compare(a.Activity)
	})
	return sessions
}
