# devboard

A local dashboard of what is waiting on you across your Claude desktop sessions and git worktrees.

Each line is a branch. Under it you find its worktree, its pull request and every Claude session working on it, whether that's the one writing the code, the one addressing review comments, or the one doing your own review. Lines that need you come first:

- a Claude session asked you a question or is blocked on you;
- a Claude session finished work that is ready for review.

Sessions tied to no branch (other projects, sessions on the base branch) are listed apart.

One click opens a session in the Claude desktop app, a pull request on GitHub, or a worktree in Finder or your editor.

## How it works

devboard is a single Go binary with no dependencies. It reads local files and never writes them:

| Source | What is read | Refresh |
|---|---|---|
| Claude desktop app | `~/Library/Application Support/Claude/claude-code-sessions/**/local_*.json`: title, state, linked pull requests, branches written | every 3 s, only changed files |
| Claude Code transcripts | the last `gitBranch` of `~/.claude/projects/*/<session>.jsonl` | with the session |
| Git worktrees | `wt list --format=json` ([worktrunk](https://worktrunk.dev)), falling back to `git worktree list` | every 30 s |

The page receives updates through server-sent events, so it stays live without reloading. It also refreshes when the tab gets the focus again.

Sessions open through the app's `claude://code/continue?session=<id>` link.

The session records are an internal format of the Claude desktop app. They can change with any app update, and devboard will need to follow.

## Run

Requires macOS, the Claude desktop app and Go 1.23+.

```bash
go run .
```

Then open http://127.0.0.1:7777.

## Configure

The config lives in `~/.config/devboard/config.json`, outside the repository, since it holds your local paths. Without it, devboard shows sessions only. See [config.example.json](config.example.json):

| Key | Default | |
|---|---|---|
| `repos` | none | repositories whose worktrees are tracked, each with the `base_branch` work merges into |
| `addr` | `127.0.0.1:7777` | keep it on loopback |
| `session_max_age_days` | `14` | hides sessions inactive for longer |
| `editor_command` | `["code"]` | command that opens a worktree; the path is appended |
| `sessions_refresh_seconds` | `3` | |
| `worktrees_refresh_seconds` | `30` | |

## Security

The server only answers on the loopback interface and rejects any other `Host` header, which blocks DNS rebinding. Actions (refresh, open a folder) require an `X-Devboard` header, which a cross-site page cannot send. Folders can be opened only if they are a known worktree.

## Roadmap

- GitHub pull requests: review requested, changes requested, unresolved threads, failing CI, conflicts.
- Pending tool permissions and Claude Code CLI sessions.
- Notion tickets and Slack mentions.
- Start a Claude session from a ticket or a mention (`claude://code/new?q=…&folder=…`).
- A `launchd` agent to keep it running.
