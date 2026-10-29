package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Notion tickets come from one database, read through the public API with an
// internal integration token. 2022-06-28 is the last API version whose
// database endpoints serve a database with a single data source directly.
const (
	notionAPI     = "https://api.notion.com/v1"
	notionVersion = "2022-06-28"
)

type NotionConfig struct {
	// DatabaseID is the tickets database, shared with the integration.
	DatabaseID string `json:"database_id"`
	// UserID is my Notion user id: tickets assigned to it are mine.
	UserID string `json:"user_id"`
	// AssigneeProperty is the people property holding who a ticket is
	// assigned to.
	AssigneeProperty string `json:"assignee_property"`
	// StatusProperty defaults to the database's first status property.
	StatusProperty string `json:"status_property"`
	// PriorityProperty is an optional select property; its options are
	// ranked in the order the database lists them, most urgent first.
	PriorityProperty string `json:"priority_property"`
	// TokenCommand prints the integration token, e.g. a Keychain lookup.
	// Without it the token is read from NOTION_TOKEN.
	TokenCommand []string `json:"token_command"`
	// OpenInApp opens tickets in the Notion desktop app instead of the
	// browser.
	OpenInApp bool `json:"open_in_app"`
	// RefreshSeconds polls Notion; 0 fetches only on a refresh.
	RefreshSeconds int64 `json:"refresh_seconds"`
}

func (c NotionConfig) enabled() bool { return c.DatabaseID != "" && c.UserID != "" }

// Ticket groups follow the three fixed groups of a Notion status property.
const (
	TicketToDo  = "todo"
	TicketDoing = "doing"
)

type Ticket struct {
	// ID is the database's unique ID ("UL-3884"), if it has one.
	ID       string    `json:"id,omitempty"`
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Status   string    `json:"status"`
	Group    string    `json:"group"`
	Priority string    `json:"priority,omitempty"`
	Edited   time.Time `json:"edited"`
	// Backlog marks a ticket in the first to-do status: not ready to start.
	Backlog bool `json:"backlog,omitempty"`
	// PRs are the pull requests linked from the ticket's page.
	PRs       []PR     `json:"prs,omitempty"`
	Attention []string `json:"attention,omitempty"`

	pageID       string
	statusRank   int
	priorityRank int
}

type notionClient struct {
	base   string
	cfg    NotionConfig
	client *http.Client
	token  string
	schema *notionSchema
	// bodies caches the pull requests linked from a page, by page id, until
	// the page is edited.
	bodies map[string]cachedBody
}

type cachedBody struct {
	edited time.Time
	prs    []PR
}

// notionSchema is what devboard needs to know about the database's
// properties.
type notionSchema struct {
	title, id, idPrefix, status, priority string
	statusGroup                           map[string]string
	statusRank                            map[string]int
	firstToDo                             string
	complete                              []string
	priorityRank                          map[string]int
}

func newNotionClient(cfg NotionConfig) *notionClient {
	return &notionClient{
		base:   notionAPI,
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		bodies: map[string]cachedBody{},
	}
}

var errNotionAuth = errors.New("the token was refused")

func (n *notionClient) loadToken(ctx context.Context) error {
	if n.token != "" {
		return nil
	}
	if len(n.cfg.TokenCommand) == 0 {
		n.token = os.Getenv("NOTION_TOKEN")
		if n.token == "" {
			return errors.New("no token_command in the config and NOTION_TOKEN is empty")
		}
		return nil
	}
	out, err := exec.CommandContext(ctx, n.cfg.TokenCommand[0], n.cfg.TokenCommand[1:]...).Output()
	if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("token_command: %s", bytes.TrimSpace(exitErr.Stderr))
	}
	if err != nil {
		return fmt.Errorf("token_command: %w", err)
	}
	n.token = strings.TrimSpace(string(out))
	return nil
}

func (n *notionClient) call(ctx context.Context, method, path string, body any, out any) error {
	if err := n.loadToken(ctx); err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.base+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Notion-Version", notionVersion)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The token may have been rotated: read it again next time.
		n.token = ""
		return errNotionAuth
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &apiErr)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, apiErr.Message)
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

type notionOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (n *notionClient) loadSchema(ctx context.Context) error {
	if n.schema != nil {
		return nil
	}
	var db struct {
		Properties map[string]struct {
			Type   string `json:"type"`
			Status struct {
				Options []notionOption `json:"options"`
				Groups  []struct {
					OptionIDs []string `json:"option_ids"`
				} `json:"groups"`
			} `json:"status"`
			Select struct {
				Options []notionOption `json:"options"`
			} `json:"select"`
			UniqueID struct {
				Prefix string `json:"prefix"`
			} `json:"unique_id"`
		} `json:"properties"`
	}
	if err := n.call(ctx, http.MethodGet, "/databases/"+n.cfg.DatabaseID, nil, &db); err != nil {
		return err
	}

	s := &notionSchema{status: n.cfg.StatusProperty, priority: n.cfg.PriorityProperty}
	names := make([]string, 0, len(db.Properties))
	for name := range db.Properties {
		names = append(names, name)
	}
	slices.Sort(names) // map order is random; keep "first" stable
	for _, name := range names {
		p := db.Properties[name]
		switch p.Type {
		case "title":
			s.title = name
		case "unique_id":
			if s.id == "" {
				s.id, s.idPrefix = name, p.UniqueID.Prefix
			}
		case "status":
			if s.status == "" {
				s.status = name
			}
		}
	}
	if _, ok := db.Properties[n.cfg.AssigneeProperty]; !ok {
		return fmt.Errorf("no property %q in the database", n.cfg.AssigneeProperty)
	}
	status, ok := db.Properties[s.status]
	if !ok || status.Type != "status" || len(status.Status.Groups) != 3 {
		return fmt.Errorf("no status property %q with its three groups", s.status)
	}

	// A status property always has three groups, in this order: to do, in
	// progress, complete. Their names can be renamed, not their order.
	optionName := map[string]string{}
	for _, o := range status.Status.Options {
		optionName[o.ID] = o.Name
	}
	s.statusGroup = map[string]string{}
	s.statusRank = map[string]int{}
	for g, group := range status.Status.Groups {
		for i, id := range group.OptionIDs {
			name := optionName[id]
			s.statusRank[name] = i
			switch g {
			case 0:
				s.statusGroup[name] = TicketToDo
				if i == 0 {
					s.firstToDo = name
				}
			case 1:
				s.statusGroup[name] = TicketDoing
			case 2:
				s.complete = append(s.complete, name)
			}
		}
	}

	if s.priority != "" {
		p, ok := db.Properties[s.priority]
		if !ok || p.Type != "select" {
			return fmt.Errorf("no select property %q", s.priority)
		}
		s.priorityRank = map[string]int{}
		for i, o := range p.Select.Options {
			s.priorityRank[o.Name] = i
		}
	}
	n.schema = s
	return nil
}

type notionPage struct {
	ID             string                     `json:"id"`
	URL            string                     `json:"url"`
	LastEditedTime time.Time                  `json:"last_edited_time"`
	Properties     map[string]json.RawMessage `json:"properties"`
}

// fetchTickets returns my open tickets: assigned to me, in a to-do or
// in-progress status. Tickets in progress also get the pull requests their
// page links to.
func (n *notionClient) fetchTickets(ctx context.Context) ([]Ticket, error) {
	if err := n.loadSchema(ctx); err != nil {
		return nil, err
	}
	s := n.schema

	filters := []any{map[string]any{
		"property": n.cfg.AssigneeProperty,
		"people":   map[string]string{"contains": n.cfg.UserID},
	}}
	for _, name := range s.complete {
		filters = append(filters, map[string]any{
			"property": s.status,
			"status":   map[string]string{"does_not_equal": name},
		})
	}
	query := map[string]any{"filter": map[string]any{"and": filters}, "page_size": 100}

	var tickets []Ticket
	seen := map[string]bool{}
	for {
		var page struct {
			Results    []notionPage `json:"results"`
			HasMore    bool         `json:"has_more"`
			NextCursor string       `json:"next_cursor"`
		}
		if err := n.call(ctx, http.MethodPost, "/databases/"+n.cfg.DatabaseID+"/query", query, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Results {
			t := n.toTicket(p)
			if t.Group == TicketDoing {
				prs, err := n.linkedPRs(ctx, p)
				if err != nil {
					return nil, err
				}
				t.PRs = prs
			}
			seen[p.ID] = true
			tickets = append(tickets, t)
		}
		if !page.HasMore {
			break
		}
		query["start_cursor"] = page.NextCursor
	}
	for id := range n.bodies {
		if !seen[id] {
			delete(n.bodies, id)
		}
	}

	slices.SortFunc(tickets, compareTickets)
	return tickets, nil
}

func (n *notionClient) toTicket(p notionPage) Ticket {
	s := n.schema
	t := Ticket{URL: p.URL, Edited: p.LastEditedTime, pageID: p.ID, priorityRank: len(s.priorityRank)}
	if n.cfg.OpenInApp {
		if rest, ok := strings.CutPrefix(t.URL, "https://"); ok {
			t.URL = "notion://" + rest
		}
	}

	var title struct {
		Title []struct {
			PlainText string `json:"plain_text"`
		} `json:"title"`
	}
	json.Unmarshal(p.Properties[s.title], &title)
	for _, part := range title.Title {
		t.Title += part.PlainText
	}

	if s.id != "" {
		var id struct {
			UniqueID struct {
				Number *int64 `json:"number"`
			} `json:"unique_id"`
		}
		json.Unmarshal(p.Properties[s.id], &id)
		if id.UniqueID.Number != nil {
			t.ID = strconv.FormatInt(*id.UniqueID.Number, 10)
			if s.idPrefix != "" {
				t.ID = s.idPrefix + "-" + t.ID
			}
		}
	}

	var status struct {
		Status *struct {
			Name string `json:"name"`
		} `json:"status"`
	}
	json.Unmarshal(p.Properties[s.status], &status)
	if status.Status != nil {
		t.Status = status.Status.Name
	}
	t.Group = s.statusGroup[t.Status]
	t.statusRank = s.statusRank[t.Status]
	t.Backlog = t.Group == TicketToDo && t.Status == s.firstToDo

	if s.priority != "" {
		var priority struct {
			Select *struct {
				Name string `json:"name"`
			} `json:"select"`
		}
		json.Unmarshal(p.Properties[s.priority], &priority)
		if priority.Select != nil {
			t.Priority = priority.Select.Name
			t.priorityRank = s.priorityRank[t.Priority]
		}
	}
	return t
}

var githubPRLink = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)`)

// linkedPRs returns the pull requests linked from the top-level blocks of a
// page, whatever their form: a link, a mention, a bookmark or a bare URL.
func (n *notionClient) linkedPRs(ctx context.Context, p notionPage) ([]PR, error) {
	if cached, ok := n.bodies[p.ID]; ok && cached.edited.Equal(p.LastEditedTime) {
		return cached.prs, nil
	}
	var body []byte
	if err := n.call(ctx, http.MethodGet, "/blocks/"+p.ID+"/children?page_size=100", nil, &body); err != nil {
		return nil, err
	}
	prs := parsePRLinks(body)
	n.bodies[p.ID] = cachedBody{edited: p.LastEditedTime, prs: prs}
	return prs, nil
}

func parsePRLinks(text []byte) []PR {
	var prs []PR
	seen := map[string]bool{}
	for _, m := range githubPRLink.FindAllSubmatch(text, -1) {
		repo, number := string(m[1]), string(m[2])
		url := "https://github.com/" + repo + "/pull/" + number
		if seen[url] {
			continue
		}
		seen[url] = true
		n, _ := strconv.ParseInt(number, 10, 64)
		prs = append(prs, PR{Number: n, Repo: repo, URL: url})
	}
	return prs
}

// compareTickets puts tickets in progress first, then the ones furthest
// along their group, then the most urgent, then the most recently edited.
func compareTickets(a, b Ticket) int {
	if a.Group != b.Group {
		if a.Group == TicketDoing {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(b.statusRank, a.statusRank); c != 0 {
		return c
	}
	if c := cmp.Compare(a.priorityRank, b.priorityRank); c != 0 {
		return c
	}
	return b.Edited.Compare(a.Edited)
}

// mentionsTicket reports whether text names the ticket ID, as in a branch
// "fix/api/ul-3884-..." or a PR title "... (UL-3884)", but not UL-38840.
func mentionsTicket(text, id string) bool {
	text, id = strings.ToLower(text), strings.ToLower(id)
	for i := 0; ; {
		j := strings.Index(text[i:], id)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(id)
		before := start == 0 || !isAlnum(text[start-1])
		after := end == len(text) || text[end] < '0' || text[end] > '9'
		if before && after {
			return true
		}
		i = start + 1
	}
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}
