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
	// GitHub is false until pull requests could be fetched once.
	GitHub    bool      `json:"github"`
	Errors    []string  `json:"errors,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PR state for a pull request a session record still sees open but GitHub no
// longer lists among my open ones: merged or closed, without saying which.
const prStateDone = "DONE"

func buildBoard(sessions []Session, worktrees []Worktree, gh *GitHub, errs []string, now time.Time) Board {
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

	board := Board{Loose: loose, GitHub: gh != nil, Errors: errs, UpdatedAt: now}
	for _, u := range units {
		u.Sessions = sortSessions(u.Sessions)
		open := u.PR != nil && u.PR.State == "OPEN"
		// A branch with no worktree and no open PR is gone or done: its idle
		// sessions are only history.
		if u.Worktree == nil && !open && (len(u.Sessions) == 0 || u.Sessions[0].State == StateIdle) {
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
		if dir == p || strings.HasPrefix(dir, p+string(filepath.Separator)) {
			if best == nil || len(p) > len(best.Path) {
				best = &worktrees[i]
			}
		}
	}
	return best
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
