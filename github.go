package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GitHub data comes from one GraphQL query run through the gh CLI, which
// reuses its authentication. The connection sizes are kept small: GitHub
// charges rate-limit points by the number of nodes a query can return, and
// this one costs about 10 points out of 5,000 an hour.
const githubQuery = `
query($reviewQuery: String!) {
  viewer {
    login
    open: pullRequests(first: 30, states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}) { nodes { ...full } }
    done: pullRequests(first: 30, states: [MERGED, CLOSED], orderBy: {field: UPDATED_AT, direction: DESC}) { nodes { ...base } }
  }
  requested: search(query: $reviewQuery, type: ISSUE, first: 20) { nodes { ... on PullRequest { ...base isDraft author { login } } } }
}
fragment base on PullRequest { number title url state headRefName repository { nameWithOwner } }
fragment full on PullRequest {
  ...base isDraft reviewDecision mergeable
  commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
  reviewThreads(last: 30) { nodes { isResolved comments(last: 1) { nodes { author { login } } } } }
}`

type ghPR struct {
	Number         int64  `json:"number"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	State          string `json:"state"`
	HeadRefName    string `json:"headRefName"`
	IsDraft        bool   `json:"isDraft"`
	ReviewDecision string `json:"reviewDecision"`
	Mergeable      string `json:"mergeable"`
	Repository     struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ReviewThreads struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
			Comments   struct {
				Nodes []struct {
					Author *struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
}

type ghResponse struct {
	Data struct {
		Viewer struct {
			Login string `json:"login"`
			Open  struct {
				Nodes []ghPR `json:"nodes"`
			} `json:"open"`
			Done struct {
				Nodes []ghPR `json:"nodes"`
			} `json:"done"`
		} `json:"viewer"`
		Requested struct {
			Nodes []ghPR `json:"nodes"`
		} `json:"requested"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// GitHub is what devboard knows from GitHub at one point in time.
type GitHub struct {
	// Open are my open pull requests and those whose review is requested
	// from me.
	Open []PR
	// Done are my recently merged or closed pull requests: they tell a
	// worktree whose work already landed from one that never had a PR.
	Done []PR
}

func fetchGitHub(ctx context.Context, reviewQuery string) (*GitHub, error) {
	out, err := run(ctx, "", "gh", "api", "graphql",
		"-f", "query="+githubQuery,
		"-f", "reviewQuery="+reviewQuery)
	if err != nil {
		return nil, err
	}
	var resp ghResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse GitHub response: %w", err)
	}
	if len(resp.Errors) > 0 {
		var msgs []string
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("GitHub: %s", strings.Join(msgs, "; "))
	}

	me := resp.Data.Viewer.Login
	gh := &GitHub{}
	seen := map[string]bool{}
	for _, p := range resp.Data.Viewer.Open.Nodes {
		pr := p.toPR(me)
		pr.Mine = true
		gh.Open = append(gh.Open, pr)
		seen[pr.URL] = true
	}
	for _, p := range resp.Data.Requested.Nodes {
		if p.URL == "" || seen[p.URL] {
			continue
		}
		pr := p.toPR(me)
		pr.ReviewRequested = true
		gh.Open = append(gh.Open, pr)
	}
	for _, p := range resp.Data.Viewer.Done.Nodes {
		gh.Done = append(gh.Done, p.toPR(me))
	}
	return gh, nil
}

func (p ghPR) toPR(me string) PR {
	pr := PR{
		Number:         p.Number,
		Repo:           p.Repository.NameWithOwner,
		URL:            p.URL,
		State:          p.State,
		Branch:         p.HeadRefName,
		Title:          p.Title,
		Draft:          p.IsDraft,
		ReviewDecision: p.ReviewDecision,
		Mergeable:      p.Mergeable,
	}
	if p.Author != nil {
		pr.Author = p.Author.Login
	}
	if len(p.Commits.Nodes) > 0 {
		if rollup := p.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
			pr.Checks = rollup.State
		}
	}
	// A thread waits on me while it is unresolved and someone else spoke
	// last; my own reply means the ball is in the reviewer's court.
	for _, t := range p.ReviewThreads.Nodes {
		if t.IsResolved || len(t.Comments.Nodes) == 0 {
			continue
		}
		if a := t.Comments.Nodes[0].Author; a == nil || a.Login != me {
			pr.UnresolvedThreads++
		}
	}
	return pr
}

// prAttention lists what an open pull request waits on me for.
func prAttention(pr PR) []string {
	if pr.ReviewRequested {
		return []string{"Your review is requested"}
	}
	var reasons []string
	if pr.ReviewDecision == "CHANGES_REQUESTED" {
		reasons = append(reasons, "Changes requested")
	}
	if pr.UnresolvedThreads > 0 {
		reasons = append(reasons, plural(pr.UnresolvedThreads, "unresolved thread"))
	}
	if pr.Checks == "FAILURE" || pr.Checks == "ERROR" {
		reasons = append(reasons, "CI failing")
	}
	if pr.Mergeable == "CONFLICTING" {
		reasons = append(reasons, "Merge conflict")
	}
	if len(reasons) == 0 && !pr.Draft && pr.ReviewDecision == "APPROVED" &&
		pr.Mergeable == "MERGEABLE" && (pr.Checks == "" || pr.Checks == "SUCCESS") {
		reasons = append(reasons, "Ready to merge")
	}
	return reasons
}

func plural(n int64, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// staleWorkAfter is how long a worktree with unpushed or unreviewed work can
// sit without a pull request before it is flagged.
const staleWorkAfter = 3 * 24 * time.Hour
