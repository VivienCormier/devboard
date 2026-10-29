package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, lines ...string) transcript {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return readTranscript(transcript{}, path, info)
}

const (
	userText    = `{"type":"user","gitBranch":"feat/x","message":{"content":"go"}}`
	bashCall    = `{"type":"assistant","gitBranch":"feat/x","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make test","description":"Run the tests"}}]}}`
	bashResult  = `{"type":"user","gitBranch":"feat/x","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}`
	questionUse = `{"type":"assistant","gitBranch":"feat/y","message":{"content":[{"type":"tool_use","id":"t2","name":"AskUserQuestion","input":{"questions":[{"question":"Which fix first?"}]}}]}}`
	subagentUse = `{"type":"assistant","isSidechain":true,"gitBranch":"feat/z","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{}}]}}`
)

func TestReadTranscript(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		branch     string
		pending    string
		detail     string
		isQuestion bool
	}{
		{name: "answered call", lines: []string{userText, bashCall, bashResult}, branch: "feat/x"},
		{name: "call waiting", lines: []string{userText, bashCall}, branch: "feat/x", pending: "Bash", detail: "Run the tests"},
		{name: "question wins", lines: []string{questionUse, bashCall}, branch: "feat/x", pending: "AskUserQuestion", detail: "Which fix first?", isQuestion: true},
		{name: "subagent ignored", lines: []string{userText, subagentUse}, branch: "feat/x"},
		{name: "cut first line", lines: []string{`"tool_use","id":"t9"}]}}`, userText}, branch: "feat/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := writeTranscript(t, tt.lines...)
			if got.branch != tt.branch {
				t.Errorf("branch = %q, want %q", got.branch, tt.branch)
			}
			name, detail := "", ""
			if got.pending != nil {
				name, detail = got.pending.name, got.pending.detail
			}
			if name != tt.pending || detail != tt.detail {
				t.Errorf("pending = %q %q, want %q %q", name, detail, tt.pending, tt.detail)
			}
			if got.pending.isQuestion() != tt.isQuestion {
				t.Errorf("isQuestion = %v, want %v", got.pending.isQuestion(), tt.isQuestion)
			}
		})
	}
}

func TestPendingToolState(t *testing.T) {
	now := time.Now()
	open := desktopSession{SessionID: "local_1", PermissionMode: "auto", LastActivityAt: now.UnixMilli(), LastTurnEdgeAt: now.UnixMilli(), LatestUserFrameAt: now.UnixMilli()}
	bash := &pendingTool{name: "Bash", detail: "Run the tests"}
	edit := &pendingTool{name: "Edit", detail: "main.go"}
	question := &pendingTool{name: "AskUserQuestion", detail: "Which fix first?"}

	tests := []struct {
		name     string
		mode     string
		pending  *pendingTool
		age      time.Duration
		state    string
		approval bool
	}{
		{name: "question", pending: question, state: StateWaiting},
		{name: "recent call", pending: bash, age: 5 * time.Second, state: StateWorking},
		{name: "stalled call", pending: bash, age: time.Minute, state: StateWaiting, approval: true},
		{name: "edit in auto mode", pending: edit, age: time.Minute, state: StateWorking},
		{name: "edit in default mode", mode: "default", pending: edit, age: time.Minute, state: StateWaiting, approval: true},
		{name: "bypass", mode: "bypassPermissions", pending: bash, age: time.Minute, state: StateWorking},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := open
			if tt.mode != "" {
				d.PermissionMode = tt.mode
			}
			s := d.toSession(now, transcript{mtime: now.Add(-tt.age), pending: tt.pending})
			if s.State != tt.state || s.Approval != tt.approval {
				t.Errorf("state = %s approval = %v, want %s %v", s.State, s.Approval, tt.state, tt.approval)
			}
		})
	}
}

func TestReadTranscriptIncrementally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	write := func(content string) os.FileInfo {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(content)
		f.Close()
		info, _ := os.Stat(path)
		return info
	}
	edit := `{"type":"assistant","gitBranch":"feat/x","message":{"content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/code/app/main.go"}}]}}`
	memory := `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"tool_use","id":"e2","name":"Write","input":{"file_path":"` + os.Getenv("HOME") + `/.claude/memory/note.md"}}]}}`

	// The last line is still being written: it is read once complete.
	got := readTranscript(transcript{}, path, write(userText+"\n"+bashCall+"\n"+edit[:40]))
	if got.pending == nil || got.pending.name != "Bash" {
		t.Fatalf("pending = %+v, want Bash", got.pending)
	}
	got = readTranscript(got, path, write(edit[40:]+"\n"+memory+"\n"+bashResult+"\n"))
	if got.pending == nil || got.pending.name != "Edit" {
		t.Errorf("pending = %+v, want the Edit still open", got.pending)
	}
	if !got.edited["/code/app/main.go"] || len(got.edited) != 1 {
		t.Errorf("edited = %v, want main.go only, not Claude's own files", got.edited)
	}

	// A rewritten file is read again from the start.
	os.WriteFile(path, []byte(userText+"\n"), 0o600)
	info, _ := os.Stat(path)
	got = readTranscript(got, path, info)
	if got.pending != nil || len(got.edited) != 0 {
		t.Errorf("after a rewrite: pending %+v, edited %v", got.pending, got.edited)
	}
}
