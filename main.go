// Command devboard serves a local dashboard of what is waiting on you across
// Claude desktop sessions, git worktrees and pull requests.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to the config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if len(cfg.Repos) == 0 {
		log.Printf("no repos in %s: worktrees are not tracked", *configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := newCollector(cfg)
	go c.run(ctx)

	srv := &server{cfg: cfg, hub: c.hub, refresh: c.refreshAll, paths: c.worktreePaths}
	if err := serve(ctx, cfg.Addr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}

// collector refreshes each source at its own pace and republishes the board
// whenever one of them changes.
type collector struct {
	cfg      Config
	hub      *hub
	sessions *sessionScanner

	mu           sync.Mutex
	lastSessions []Session
	worktrees    []Worktree
	worktreesErr error
	github       *GitHub
	githubErr    error
	githubAt     time.Time
	tickets      []Ticket
	ticketsErr   error
	ticketsAt    time.Time

	notion *notionClient

	wakeWorktrees chan struct{}
	wakeGitHub    chan struct{}
	wakeNotion    chan struct{}
}

func newCollector(cfg Config) *collector {
	c := &collector{
		cfg:           cfg,
		hub:           newHub(),
		sessions:      newSessionScanner(cfg.ClaudeSessionsDir, cfg.ClaudeProjectsDir, cfg.sessionMaxAge()),
		wakeWorktrees: make(chan struct{}, 1),
		wakeGitHub:    make(chan struct{}, 1),
		wakeNotion:    make(chan struct{}, 1),
	}
	if cfg.Notion.enabled() {
		c.notion = newNotionClient(cfg.Notion)
	}
	return c
}

func (c *collector) run(ctx context.Context) {
	go c.loop(ctx, seconds(c.cfg.WorktreesRefreshSeconds), c.wakeWorktrees, c.collectWorktrees)
	go c.loop(ctx, seconds(c.cfg.GitHubRefreshSeconds), c.wakeGitHub, c.collectGitHub)
	if c.notion != nil {
		go c.loop(ctx, seconds(c.cfg.Notion.RefreshSeconds), c.wakeNotion, c.collectTickets)
	}

	// Sessions are cheap to rescan, so they are polled and need no wake-up.
	c.collectSessions(ctx)
	ticker := time.NewTicker(seconds(c.cfg.SessionsRefreshSeconds))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collectSessions(ctx)
		}
	}
}

func (c *collector) loop(ctx context.Context, every time.Duration, wake <-chan struct{}, collect func(context.Context)) {
	collect(ctx)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
		collect(ctx)
	}
}

func (c *collector) refreshAll() {
	for _, wake := range []chan struct{}{c.wakeWorktrees, c.wakeGitHub, c.wakeNotion} {
		select {
		case wake <- struct{}{}:
		default: // a refresh is already pending
		}
	}
}

// minFetchInterval spaces out GitHub and Notion fetches triggered by the
// page (refresh button, tab focus), which would otherwise burn rate limits.
const minFetchInterval = 15 * time.Second

func (c *collector) collectGitHub(ctx context.Context) {
	c.mu.Lock()
	recent := time.Since(c.githubAt) < minFetchInterval
	c.mu.Unlock()
	if recent {
		return
	}

	gh, err := fetchGitHub(ctx, c.cfg.GitHubReviewQuery)
	if err != nil {
		log.Printf("github: %v", err)
	}
	c.mu.Lock()
	c.githubAt = time.Now()
	c.githubErr = err
	// Keep the last good data through a transient failure.
	if gh != nil {
		c.github = gh
	}
	c.mu.Unlock()
	c.publish()
}

// collectTickets runs on its own goroutine, the only one using the Notion
// client.
func (c *collector) collectTickets(ctx context.Context) {
	c.mu.Lock()
	recent := time.Since(c.ticketsAt) < minFetchInterval
	c.mu.Unlock()
	if recent {
		return
	}

	tickets, err := c.notion.fetchTickets(ctx)
	if err != nil {
		log.Printf("notion: %v", err)
	}
	c.mu.Lock()
	c.ticketsAt = time.Now()
	c.ticketsErr = err
	// Keep the last good tickets through a transient failure.
	// A non-nil slice tells the board Notion answered, even with no tickets.
	if err == nil {
		if tickets == nil {
			tickets = []Ticket{}
		}
		c.tickets = tickets
	}
	c.mu.Unlock()
	c.publish()
}

func (c *collector) collectSessions(context.Context) {
	sessions := c.sessions.scan(time.Now())
	c.mu.Lock()
	c.lastSessions = sessions
	c.mu.Unlock()
	c.publish()
}

func (c *collector) collectWorktrees(ctx context.Context) {
	wts, err := listWorktrees(ctx, c.cfg.Repos)
	if err != nil {
		log.Printf("worktrees: %v", err)
	}
	c.mu.Lock()
	c.worktrees = wts
	c.worktreesErr = err
	c.mu.Unlock()
	c.publish()
}

func (c *collector) publish() {
	c.mu.Lock()
	var errs []string
	if c.worktreesErr != nil {
		errs = append(errs, "worktrees: "+c.worktreesErr.Error())
	}
	if c.githubErr != nil {
		errs = append(errs, "GitHub: "+c.githubErr.Error())
	}
	if c.ticketsErr != nil {
		errs = append(errs, "Notion: "+c.ticketsErr.Error())
	}
	board := buildBoard(append([]Session(nil), c.lastSessions...), c.worktrees, c.github, c.tickets, errs, time.Now())
	c.mu.Unlock()
	c.hub.publish(board)
}

func (c *collector) worktreePaths() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	paths := map[string]bool{}
	for _, wt := range c.worktrees {
		paths[wt.Path] = true
	}
	return paths
}
