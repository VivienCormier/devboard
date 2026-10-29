package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// transcript is what devboard reads from a session's CLI transcript
// (~/.claude/projects/<encoded cwd>/<cli session id>.jsonl). The file only
// grows: it is read once, then only what was appended since.
type transcript struct {
	path  string
	mtime time.Time
	// size is how far the file was read, up to its last complete line.
	size int64
	// branch is the branch checked out in cwd at the last message.
	branch string
	// pending is the last tool call of the main thread still waiting for its
	// result, if any.
	pending *pendingTool
	// open are the main thread's tool calls without a result yet, by id.
	open map[string]*pendingTool
	seq  int
	// edited are the files the session changed with its edit tools.
	edited map[string]bool
}

type pendingTool struct {
	name string
	// detail is the question asked, or what the tool call is about.
	detail string
	seq    int
}

// questionTools wait for an answer from the person by design: a call to one
// of them that has no result yet is a question waiting for an answer.
var questionTools = map[string]bool{"AskUserQuestion": true, "ExitPlanMode": true}

func (p *pendingTool) isQuestion() bool {
	return p != nil && questionTools[p.name]
}

// mayAskApproval reports whether a call to the tool can stop on a permission
// prompt under the session's permission mode. Tools that only read, and the
// app's own tools, never ask.
func (p *pendingTool) mayAskApproval(permissionMode string) bool {
	if p == nil {
		return false
	}
	switch p.name {
	case "Bash", "WebFetch":
		return permissionMode != "bypassPermissions"
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return permissionMode == "default" || permissionMode == "plan"
	}
	return strings.HasPrefix(p.name, "mcp__") && !strings.HasPrefix(p.name, "mcp__ccd_") &&
		permissionMode != "bypassPermissions"
}

type transcriptLine struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	GitBranch   string `json:"gitBranch"`
	Message     struct {
		// Content is a string for a plain user message.
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

// editTools change files; their input names the file.
var editTools = map[string]bool{"Edit": true, "MultiEdit": true, "Write": true, "NotebookEdit": true}

// maxEdited bounds the edited files remembered per session.
const maxEdited = 500

// readTranscript reads what was appended to a transcript since prev was
// read, or the whole file when it is new or was rewritten.
func readTranscript(prev transcript, path string, info os.FileInfo) transcript {
	t := prev
	if t.path != path || info.Size() < t.size {
		t = transcript{path: path}
	}
	t.mtime = info.ModTime()
	// The maps are shared with sessions built from prev: copy them.
	t.open = maps.Clone(t.open)
	t.edited = maps.Clone(t.edited)
	if t.open == nil {
		t.open = map[string]*pendingTool{}
	}
	if t.edited == nil {
		t.edited = map[string]bool{}
	}

	f, err := os.Open(path)
	if err != nil {
		return t
	}
	defer f.Close()
	if _, err := f.Seek(t.size, io.SeekStart); err != nil {
		return t
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a line still being written is read next time
		}
		t.size += int64(len(line))
		t.readLine(line)
	}

	// The latest question wins over a call that may be waiting for an
	// approval, then the latest call.
	t.pending = nil
	for _, p := range t.open {
		switch {
		case t.pending == nil,
			p.isQuestion() && !t.pending.isQuestion(),
			p.isQuestion() == t.pending.isQuestion() && p.seq > t.pending.seq:
			t.pending = p
		}
	}
	return t
}

func (t *transcript) readLine(data []byte) {
	var line transcriptLine
	if json.Unmarshal(data, &line) != nil || line.Type != "assistant" && line.Type != "user" {
		return
	}
	if line.GitBranch != "" && !line.IsSidechain {
		t.branch = line.GitBranch
	}
	var blocks []contentBlock
	if json.Unmarshal(line.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			// A subagent's edits are the session's work too.
			if editTools[b.Name] && len(t.edited) < maxEdited {
				if path := editedFile(b.Input); path != "" {
					t.edited[path] = true
				}
			}
			if !line.IsSidechain {
				t.seq++
				t.open[b.ID] = &pendingTool{name: b.Name, detail: toolDetail(b), seq: t.seq}
			}
		case "tool_result":
			delete(t.open, b.ToolUseID)
		}
	}
}

// editedFile returns the file an edit tool changed, unless it is one of
// Claude's own files (memory, plans) or a temporary file: those say
// nothing about the code the session works on.
func editedFile(input json.RawMessage) string {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	json.Unmarshal(input, &in)
	path := cmp.Or(in.FilePath, in.NotebookPath)
	if !filepath.IsAbs(path) {
		return ""
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(home, ".claude"), "/tmp", "/private", "/var/folders"} {
		if strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return ""
		}
	}
	return filepath.Clean(path)
}

func toolDetail(b contentBlock) string {
	var input struct {
		Description string `json:"description"`
		Command     string `json:"command"`
		FilePath    string `json:"file_path"`
		URL         string `json:"url"`
		Questions   []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	json.Unmarshal(b.Input, &input)
	switch {
	case b.Name == "ExitPlanMode":
		return "Plan waiting for your approval"
	case len(input.Questions) > 0:
		return input.Questions[0].Question
	case input.Description != "":
		return input.Description
	case input.FilePath != "":
		return filepath.Base(input.FilePath)
	case input.URL != "":
		return input.URL
	}
	command, _, _ := strings.Cut(input.Command, "\n")
	return command
}

// transcript returns the session's parsed transcript, rereading it only when
// it changed.
func (sc *sessionScanner) transcript(d desktopSession) transcript {
	if d.CLISessionID == "" {
		return transcript{}
	}
	cached := sc.transcripts[d.CLISessionID]
	path := cached.path
	if path == "" {
		path = filepath.Join(sc.projectsDir, encodeProjectDir(d.Cwd), d.CLISessionID+".jsonl")
		if _, err := os.Stat(path); err != nil {
			matches, _ := filepath.Glob(filepath.Join(sc.projectsDir, "*", d.CLISessionID+".jsonl"))
			if len(matches) == 0 {
				return transcript{}
			}
			path = matches[0]
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		delete(sc.transcripts, d.CLISessionID)
		return transcript{}
	}
	if cached.path == path && cached.mtime.Equal(info.ModTime()) {
		return cached
	}
	t := readTranscript(cached, path, info)
	sc.transcripts[d.CLISessionID] = t
	return t
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
