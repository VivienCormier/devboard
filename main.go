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

	srv := &server{cfg: cfg, hub: c.hub, refresh: c.refresh, paths: c.worktreePaths}
	if err := serve(ctx, cfg.Addr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}

// collector fetches the sources when the page asks for a refresh, and keeps
// rescanning the local session records so the page can tell that something
// changed. GitHub, Notion and the worktrees are also polled when the config
// sets an interval for them.
type collector struct {
	cfg      Config
	hub      *hub
	sessions *sessionScanner
	notion   *notionClient

	// Each source is fetched by one caller at a time: a refresh arriving
	// during a fetch waits for it, then finds it recent enough.
	scanMu, githubMu, notionMu sync.Mutex

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
}

func newCollector(cfg Config) *collector {
	c := &collector{
		cfg:      cfg,
		hub:      newHub(),
		sessions: newSessionScanner(cfg.ClaudeSessionsDir, cfg.ClaudeProjectsDir, cfg.sessionMaxAge()),
	}
	if cfg.Notion.enabled() {
		c.notion = newNotionClient(cfg.Notion)
	}
	return c
}

func (c *collector) run(ctx context.Context) {
	go c.every(ctx, c.cfg.WorktreesRefreshSeconds, c.collectWorktrees)
	go c.every(ctx, c.cfg.GitHubRefreshSeconds, c.collectGitHub)
	if c.notion != nil {
		go c.every(ctx, c.cfg.Notion.RefreshSeconds, c.collectTickets)
	}

	// Session records are local files, cheap to rescan: they are polled.
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

// every collects once, then every n seconds. With n = 0 the source is
// fetched again only when the page asks for a refresh.
func (c *collector) every(ctx context.Context, n int64, collect func(context.Context)) {
	collect(ctx)
	if n <= 0 {
		return
	}
	ticker := time.NewTicker(seconds(n))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect(ctx)
		}
	}
}

// refresh fetches every source and returns the resulting board.
func (c *collector) refresh(ctx context.Context) Board {
	collects := []func(context.Context){c.collectWorktrees, c.collectGitHub, c.collectSessions}
	if c.notion != nil {
		collects = append(collects, c.collectTickets)
	}
	var wg sync.WaitGroup
	for _, collect := range collects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collect(ctx)
		}()
	}
	wg.Wait()
	return c.publish()
}

// minFetchInterval spaces out GitHub and Notion fetches, which would
// otherwise burn rate limits on repeated clicks.
const minFetchInterval = 15 * time.Second

func (c *collector) collectGitHub(ctx context.Context) {
	c.githubMu.Lock()
	defer c.githubMu.Unlock()
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

// collectTickets holds notionMu: the Notion client caches are not safe for
// concurrent use.
func (c *collector) collectTickets(ctx context.Context) {
	c.notionMu.Lock()
	defer c.notionMu.Unlock()
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
	c.scanMu.Lock()
	sessions := c.sessions.scan(time.Now())
	c.scanMu.Unlock()
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

func (c *collector) publish() Board {
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
	return board
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
