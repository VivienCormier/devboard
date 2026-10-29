# devboard

A local dashboard of what is waiting on you across your Claude desktop sessions, git worktrees, GitHub pull requests and Notion tickets.

Each line is a branch. Under it you find its worktree, its pull request, its ticket and every Claude session working on it, whether that's the one writing the code, the one addressing review comments, or the one doing your own review. Lines that need you come first:

- a Claude session asked you a question or is blocked on you;
- a Claude session seems stopped on a tool permission prompt;
- a Claude session finished work that is ready for review;
- your pull request has changes requested, unresolved review threads where someone else spoke last, a failing CI or a merge conflict;
- your pull request is approved, green and mergeable;
- a worktree holds uncommitted or unpushed work, has no pull request and has been idle for 3 days;
- a ticket is still in progress although its pull request was merged.

Pull requests waiting on your review get their own section, and so do your tickets tied to no branch: the ones in progress, then the ones to start, with the backlog folded. Sessions tied to no live branch (other projects, sessions on the base branch, work that already landed) are listed apart.

One click opens a session in the Claude desktop app, a pull request on GitHub, a ticket in Notion, or a worktree in Finder or your editor.

## How it works

devboard is a single Go binary with no dependencies. It reads local files, never writes them, and asks GitHub through the `gh` CLI:

| Source | What is read | Fetched |
|---|---|---|
| Claude desktop app | `~/Library/Application Support/Claude/claude-code-sessions/**/local_*.json`: title, state, linked pull requests, branches written | every 3 s, only changed files |
| Claude Code transcripts | the end of `~/.claude/projects/*/<session>.jsonl`: the last `gitBranch`, and the tool calls still waiting for their result | with the session, only changed files |
| Git worktrees | `wt list --format=json` ([worktrunk](https://worktrunk.dev)), falling back to `git worktree list` | on refresh |
| GitHub | one GraphQL query through `gh api graphql`: your open and recently closed pull requests, and those waiting on your review | on refresh |
| Notion | the tickets database through the API: tickets assigned to you that are not complete, and the pull requests linked from the page of those in progress | on refresh, pages only when edited |

The page changes only when you click Refresh or reload it, so cards never move while you read them. A refresh fetches every source and shows the result. GitHub and Notion are fetched at most every 15 s, however often you click. The GitHub query costs about 10 rate-limit points out of the 5,000 an hour GitHub allows.

Between refreshes, devboard keeps rescanning the local session records, which costs nothing. When something that needs you changes, the Refresh button says so and the tab title's count follows, without touching the cards.

A pull request known from GitHub wins over what a session record remembers: a session keeps the state its pull requests had when it last looked.

A question or a permission prompt stops a turn without ending it, so the session record still describes a running turn. The transcript shows what it waits on:

- a call to `AskUserQuestion` or `ExitPlanMode` with no result is a question for you, shown as waiting with the question;
- a call to a tool that can ask for a permission (Bash, WebFetch, MCP tools; file edits only in the `default` and `plan` permission modes) that has had no result for 20 s, with nothing written to the transcript since, is shown as "approval?". A long-running command looks exactly the same, so this one is a guess.

A ticket joins a branch when the branch or its pull request title names the ticket's ID (`fix/api/ul-3884-...`, `... (UL-3884)`), or when the ticket's page links to the branch's pull request. Statuses are read from the three groups of the Notion status property (to do, in progress, complete), so devboard needs no status names. The first to-do status is treated as the backlog.

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
| `worktrees_refresh_seconds` | `0` | above 0, also polls the worktrees in the background |
| `github_refresh_seconds` | `0` | above 0, also polls GitHub in the background |
| `notion` | none | the tickets database, see below |
| `github_review_query` | `is:pr is:open review-requested:@me archived:false` | search for pull requests waiting on your review; `user-review-requested:@me` drops requests made to your teams |

### Notion

devboard reads Notion through a connection created at https://www.notion.so/profile/integrations, of either kind.

**API token** (internal connection), if your workspace lets you create one:

1. Create it with the "Read content" capability only.
2. Share the tickets database with it: on the database, **⋯ → Connections**.
3. Store its token in the Keychain, which prompts for it, and set `token_command` as in the example:

   ```bash
   security add-generic-password -a "$USER" -s devboard-notion -w
   ```

**OAuth**, when only workspace owners may create API tokens. The token acts as you, limited to the pages you pick when connecting:

1. Create it with the redirect URI `http://127.0.0.1:7777/notion/callback` (match `addr` if you changed it).
2. Put its client ID in `client_id`, and store its client secret in the Keychain, read by `client_secret_command`:

   ```bash
   security add-generic-password -a "$USER" -s devboard-notion-client -w
   ```

3. Click **Connect Notion** on the page and pick the tickets database. The tokens are kept in `notion-oauth.json` next to the config, readable by you only, and refreshed when they expire.

| Key under `notion` | |
|---|---|
| `database_id` | the tickets database, from its URL |
| `user_id` | your Notion user id: tickets assigned to it are yours |
| `assignee_property` | the people property holding who a ticket is assigned to |
| `status_property` | defaults to the database's status property |
| `priority_property` | optional select property; its options rank tickets, first is most urgent |
| `token_command` | prints an API token; without it, `NOTION_TOKEN` is read |
| `client_id` | an OAuth connection's client ID; turns OAuth on |
| `client_secret_command` | prints the OAuth client secret |
| `open_in_app` | opens tickets in the Notion desktop app instead of the browser |
| `refresh_seconds` | above 0, also polls Notion in the background; default `0` |

## Security

The server only answers on the loopback interface and rejects any other `Host` header, which blocks DNS rebinding. Actions (refresh, open a folder) require an `X-Devboard` header, which a cross-site page cannot send. Folders can be opened only if they are a known worktree. A Notion sign-in is accepted only if it was started from the page.

## Roadmap

- Claude Code CLI sessions, which have no desktop session record.
- Slack mentions.
- Start a Claude session from a ticket or a mention (`claude://code/new?q=…&folder=…`).
- A `launchd` agent to keep it running.
