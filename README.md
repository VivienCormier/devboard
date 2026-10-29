# devboard

A local dashboard of what is waiting on you across your Claude desktop sessions, git worktrees and GitHub pull requests.

Each line is a branch. Under it you find its worktree, its pull request and every Claude session working on it, whether that's the one writing the code, the one addressing review comments, or the one doing your own review. Lines that need you come first:

- a Claude session asked you a question or is blocked on you;
- a Claude session seems stopped on a tool permission prompt;
- a Claude session finished work that is ready for review;
- your pull request has changes requested, unresolved review threads where someone else spoke last, a failing CI or a merge conflict;
- your pull request is approved, green and mergeable;
- a worktree holds uncommitted or unpushed work, has no pull request and has been idle for 3 days.

Pull requests waiting on your review get their own section. Sessions tied to no live branch (other projects, sessions on the base branch, work that already landed) are listed apart.

One click opens a session in the Claude desktop app, a pull request on GitHub, or a worktree in Finder or your editor.

## How it works

devboard is a single Go binary with no dependencies. It reads local files, never writes them, and asks GitHub through the `gh` CLI:

| Source | What is read | Refresh |
|---|---|---|
| Claude desktop app | `~/Library/Application Support/Claude/claude-code-sessions/**/local_*.json`: title, state, linked pull requests, branches written | every 3 s, only changed files |
| Claude Code transcripts | the end of `~/.claude/projects/*/<session>.jsonl`: the last `gitBranch`, and the tool calls still waiting for their result | with the session, only changed files |
| Git worktrees | `wt list --format=json` ([worktrunk](https://worktrunk.dev)), falling back to `git worktree list` | every 30 s |
| GitHub | one GraphQL query through `gh api graphql`: your open and recently closed pull requests, and those waiting on your review | every 90 s |

The GitHub query costs about 10 rate-limit points out of the 5,000 an hour GitHub allows, so the default refresh uses under a tenth of it. A refresh from the page (button, tab focus) hits GitHub at most every 15 s.

A pull request known from GitHub wins over what a session record remembers: a session keeps the state its pull requests had when it last looked.

The page receives updates through server-sent events, so it stays live without reloading. It also refreshes when the tab gets the focus again.

A question or a permission prompt stops a turn without ending it, so the session record still describes a running turn. The transcript shows what it waits on:

- a call to `AskUserQuestion` or `ExitPlanMode` with no result is a question for you, shown as waiting with the question;
- a call to a tool that can ask for a permission (Bash, WebFetch, MCP tools; file edits only in the `default` and `plan` permission modes) that has had no result for 20 s, with nothing written to the transcript since, is shown as "approval?". A long-running command looks exactly the same, so this one is a guess.

Sessions open through the app's `claude://code/continue?session=<id>` link.

The session records are an internal format of the Claude desktop app. They can change with any app update, and devboard will need to follow.

## Run

Requires macOS, the Claude desktop app, Go 1.23+ and the [GitHub CLI](https://cli.github.com) logged in (`gh auth login`).

```bash
go run .
```

Then open http://127.0.0.1:7777.

## Configure

The config lives in `~/.config/devboard/config.json`, outside the repository, since it holds your local paths. Without it, devboard shows sessions and pull requests only. See [config.example.json](config.example.json):

| Key | Default | |
|---|---|---|
| `repos` | none | repositories whose worktrees are tracked, each with the `base_branch` work merges into |
| `addr` | `127.0.0.1:7777` | keep it on loopback |
| `session_max_age_days` | `14` | hides sessions inactive for longer |
| `editor_command` | `["code"]` | command that opens a worktree; the path is appended |
| `sessions_refresh_seconds` | `3` | |
| `worktrees_refresh_seconds` | `30` | |
| `github_refresh_seconds` | `90` | |
| `github_review_query` | `is:pr is:open review-requested:@me archived:false` | search for pull requests waiting on your review; `user-review-requested:@me` drops requests made to your teams |

## Security

The server only answers on the loopback interface and rejects any other `Host` header, which blocks DNS rebinding. Actions (refresh, open a folder) require an `X-Devboard` header, which a cross-site page cannot send. Folders can be opened only if they are a known worktree.

## Roadmap

- Claude Code CLI sessions, which have no desktop session record.
- Notion tickets and Slack mentions.
- Start a Claude session from a ticket or a mention (`claude://code/new?q=…&folder=…`).
- A `launchd` agent to keep it running.
