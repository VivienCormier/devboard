package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// transcriptTail bounds how much of a transcript is read: every message
// carries the branch, and a tool call waiting on its result is near the end.
const transcriptTail = 256 << 10

// transcript is what devboard reads from a session's CLI transcript
// (~/.claude/projects/<encoded cwd>/<cli session id>.jsonl).
type transcript struct {
	path  string
	mtime time.Time
	// branch is the branch checked out in cwd at the last message.
	branch string
	// pending is the last tool call of the main thread still waiting for its
	// result, if any.
	pending *pendingTool
}

type pendingTool struct {
	name string
	// detail is the question asked, or what the tool call is about.
	detail string
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

// readTranscript parses the end of a transcript. The first line of the tail
// is usually cut and is skipped.
func readTranscript(path string, info os.FileInfo) transcript {
	t := transcript{path: path, mtime: info.ModTime()}
	f, err := os.Open(path)
	if err != nil {
		return t
	}
	defer f.Close()
	offset := max(info.Size()-transcriptTail, 0)
	buf := make([]byte, info.Size()-offset)
	n, _ := f.ReadAt(buf, offset)
	buf = buf[:n]
	if offset > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}

	pending := map[string]*pendingTool{}
	var order []string
	lines := bufio.NewScanner(bytes.NewReader(buf))
	lines.Buffer(nil, transcriptTail)
	for lines.Scan() {
		var line transcriptLine
		if json.Unmarshal(lines.Bytes(), &line) != nil || line.IsSidechain {
			continue
		}
		if line.Type != "assistant" && line.Type != "user" {
			continue
		}
		if line.GitBranch != "" {
			t.branch = line.GitBranch
		}
		var blocks []contentBlock
		if json.Unmarshal(line.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				pending[b.ID] = &pendingTool{name: b.Name, detail: toolDetail(b)}
				order = append(order, b.ID)
			case "tool_result":
				delete(pending, b.ToolUseID)
			}
		}
	}

	// The latest question wins over a call that may be waiting for an
	// approval, then the latest call.
	for i := len(order) - 1; i >= 0; i-- {
		p := pending[order[i]]
		if p == nil {
			continue
		}
		if p.isQuestion() {
			t.pending = p
			break
		}
		if t.pending == nil {
			t.pending = p
		}
	}
	return t
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
	t := readTranscript(path, info)
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
