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

	wakeWorktrees chan struct{}
	wakeGitHub    chan struct{}
}

func newCollector(cfg Config) *collector {
	return &collector{
		cfg:           cfg,
		hub:           newHub(),
		sessions:      newSessionScanner(cfg.ClaudeSessionsDir, cfg.ClaudeProjectsDir, cfg.sessionMaxAge()),
		wakeWorktrees: make(chan struct{}, 1),
		wakeGitHub:    make(chan struct{}, 1),
	}
}

func (c *collector) run(ctx context.Context) {
	go c.loop(ctx, seconds(c.cfg.WorktreesRefreshSeconds), c.wakeWorktrees, c.collectWorktrees)
	go c.loop(ctx, seconds(c.cfg.GitHubRefreshSeconds), c.wakeGitHub, c.collectGitHub)

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
	for _, wake := range []chan struct{}{c.wakeWorktrees, c.wakeGitHub} {
		select {
		case wake <- struct{}{}:
		default: // a refresh is already pending
		}
	}
}

// githubMinInterval spaces out GitHub fetches triggered by the page (refresh
// button, tab focus), which would otherwise burn rate-limit points.
const githubMinInterval = 15 * time.Second

func (c *collector) collectGitHub(ctx context.Context) {
	c.mu.Lock()
	recent := time.Since(c.githubAt) < githubMinInterval
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
	board := buildBoard(append([]Session(nil), c.lastSessions...), c.worktrees, c.github, errs, time.Now())
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
