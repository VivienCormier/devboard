package main

import (
	"slices"
	"testing"
)

func TestSessionBranchesFollowEdits(t *testing.T) {
	worktrees := []Worktree{
		{Path: "/code/app", Branch: "feat/checked-out", IsMain: true},
		{Path: "/code/app.feat-other", Branch: "feat/other"},
	}
	base := map[string]bool{"main": true}
	tests := []struct {
		name   string
		edited []string
		want   []string
	}{
		{name: "no edits", want: []string{"feat/checked-out"}},
		{name: "edits in its checkout", edited: []string{"/code/app/main.go"}, want: []string{"feat/checked-out"}},
		{name: "edits in another worktree", edited: []string{"/code/app.feat-other/main.go"}, want: []string{"feat/other"}},
		{name: "edits in another repository", edited: []string{"/elsewhere/tool/main.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Session{Cwd: "/code/app/api", gitBranch: "feat/checked-out", edited: tt.edited}
			if got := sessionBranches(s, worktrees, base); !slices.Equal(got, tt.want) {
				t.Errorf("branches = %q, want %q", got, tt.want)
			}
		})
	}
}
