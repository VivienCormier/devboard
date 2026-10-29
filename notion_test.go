package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

const notionSchemaFixture = `{"properties": {
  "Ticket": {"type": "title"},
  "ID": {"type": "unique_id", "unique_id": {"prefix": "UL"}},
  "Owner": {"type": "people"},
  "State": {"type": "status", "status": {
    "options": [{"id": "b", "name": "Backlog"}, {"id": "s", "name": "Ready"}, {"id": "d", "name": "In dev"}, {"id": "r", "name": "Review"}, {"id": "x", "name": "Done"}],
    "groups": [{"option_ids": ["b", "s"]}, {"option_ids": ["d", "r"]}, {"option_ids": ["x"]}]}},
  "Priority": {"type": "select", "select": {"options": [{"name": "Hot"}, {"name": "Low"}]}}
}}`

func notionTicketFixture(id, number int, status, priority, edited string) map[string]any {
	props := map[string]any{
		"Ticket": map[string]any{"title": []any{map[string]any{"plain_text": "Ticket "}, map[string]any{"plain_text": status}}},
		"ID":     map[string]any{"unique_id": map[string]any{"number": number}},
		"State":  map[string]any{"status": map[string]any{"name": status}},
	}
	if priority != "" {
		props["Priority"] = map[string]any{"select": map[string]any{"name": priority}}
	}
	return map[string]any{
		"id":               string(rune('a' + id)),
		"url":              "https://www.notion.so/page",
		"last_edited_time": edited,
		"properties":       props,
	}
}

func TestFetchTickets(t *testing.T) {
	var queries []map[string]any
	bodies := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/databases/db":
			io.WriteString(w, notionSchemaFixture)
		case r.Method == http.MethodPost && r.URL.Path == "/databases/db/query":
			var q map[string]any
			json.NewDecoder(r.Body).Decode(&q)
			queries = append(queries, q)
			json.NewEncoder(w).Encode(map[string]any{"results": []any{
				notionTicketFixture(0, 10, "Backlog", "Hot", "2026-10-01T00:00:00Z"),
				notionTicketFixture(1, 11, "Ready", "Low", "2026-10-02T00:00:00Z"),
				notionTicketFixture(2, 12, "In dev", "", "2026-10-03T00:00:00Z"),
				notionTicketFixture(3, 13, "Review", "Low", "2026-10-04T00:00:00Z"),
			}})
		case r.Method == http.MethodGet && (r.URL.Path == "/blocks/c/children" || r.URL.Path == "/blocks/d/children"):
			bodies++
			io.WriteString(w, `{"results": [{"paragraph": {"rich_text": [{"href": "https://github.com/acme/app/pull/7"}, {"plain_text": "https://github.com/acme/app/pull/7 and https://github.com/acme/app/pull/8/files"}]}}]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("NOTION_TOKEN", "secret")

	n := newNotionClient(NotionConfig{DatabaseID: "db", UserID: "me", AssigneeProperty: "Owner", PriorityProperty: "Priority", OpenInApp: true})
	n.base = srv.URL
	tickets, err := n.fetchTickets(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, tk := range tickets {
		got = append(got, tk.ID+" "+tk.Group+" "+tk.Status)
	}
	want := []string{"UL-13 doing Review", "UL-12 doing In dev", "UL-11 todo Ready", "UL-10 todo Backlog"}
	if !slices.Equal(got, want) {
		t.Errorf("tickets = %q, want %q", got, want)
	}
	if !tickets[3].Backlog || tickets[2].Backlog {
		t.Errorf("only the first to-do status is the backlog")
	}
	if tickets[0].Title != "Ticket Review" || tickets[0].URL != "notion://www.notion.so/page" {
		t.Errorf("ticket = %+v", tickets[0])
	}
	var prs []string
	for _, pr := range tickets[0].PRs {
		prs = append(prs, pr.URL)
	}
	if want := []string{"https://github.com/acme/app/pull/7", "https://github.com/acme/app/pull/8"}; !slices.Equal(prs, want) {
		t.Errorf("prs = %q, want %q", prs, want)
	}
	if len(tickets[2].PRs) != 0 {
		t.Errorf("to-do tickets do not read their page")
	}

	filter, _ := json.Marshal(queries[0]["filter"])
	if want := `{"and":[{"people":{"contains":"me"},"property":"Owner"},{"property":"State","status":{"does_not_equal":"Done"}}]}`; string(filter) != want {
		t.Errorf("filter = %s, want %s", filter, want)
	}

	// Pages not edited since are not read again.
	if _, err := n.fetchTickets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bodies != 2 {
		t.Errorf("read %d page bodies, want 2", bodies)
	}
}

func TestMentionsTicket(t *testing.T) {
	for text, want := range map[string]bool{
		"fix/api/ul-3884-mailjet":            true,
		"feat(api): import things (UL-3884)": true,
		"feat/api/UL-38840":                  false,
		"feat/api/xul-3884":                  false,
		"feat/api/mailjet":                   false,
	} {
		if got := mentionsTicket(text, "UL-3884"); got != want {
			t.Errorf("mentionsTicket(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestBoardTickets(t *testing.T) {
	now := time.Now()
	worktrees := []Worktree{{Repo: "acme/app", Branch: "feat/ul-1-thing", Path: "/w/1"}}
	gh := &GitHub{
		Open: []PR{{Number: 5, URL: "https://github.com/acme/app/pull/5", Branch: "feat/linked", State: "OPEN", Title: "Linked"}},
		Done: []PR{{Number: 6, URL: "https://github.com/acme/app/pull/6", Branch: "feat/gone", State: "MERGED", Title: "Gone"}},
	}
	tickets := []Ticket{
		{ID: "UL-1", Group: TicketDoing, Status: "In dev"},
		{ID: "UL-2", Group: TicketDoing, Status: "In dev", PRs: []PR{{URL: "https://github.com/acme/app/pull/5"}}},
		{ID: "UL-3", Group: TicketDoing, Status: "Review", PRs: []PR{{URL: "https://github.com/acme/app/pull/6"}}},
		{ID: "UL-4", Group: TicketToDo, Status: "Ready"},
	}
	b := buildBoard(nil, worktrees, gh, tickets, nil, now)

	byBranch := map[string]Unit{}
	for _, u := range b.Units {
		byBranch[u.Branch] = u
	}
	if ts := byBranch["feat/ul-1-thing"].Tickets; len(ts) != 1 || ts[0].ID != "UL-1" {
		t.Errorf("branch naming the ticket: %+v", ts)
	}
	if ts := byBranch["feat/linked"].Tickets; len(ts) != 1 || ts[0].ID != "UL-2" {
		t.Errorf("PR linked from the ticket: %+v", ts)
	}
	var loose []string
	for _, tk := range b.Tickets {
		loose = append(loose, tk.ID)
	}
	if !slices.Equal(loose, []string{"UL-3", "UL-4"}) {
		t.Errorf("unmatched tickets = %q", loose)
	}
	if got := b.Tickets[0].Attention; !slices.Equal(got, []string{"PR merged, ticket still Review"}) {
		t.Errorf("landed ticket attention = %q", got)
	}
	if got := b.Tickets[0].PRs[0].State; got != "MERGED" {
		t.Errorf("linked PR state = %q, want MERGED", got)
	}
}

func TestBoardTicketStillOpen(t *testing.T) {
	worktrees := []Worktree{{Repo: "acme/app", Branch: "feat/first", Path: "/w/1"}, {Repo: "acme/app", Branch: "feat/second", Path: "/w/2"}}
	gh := &GitHub{
		Open: []PR{{Number: 2, URL: "https://github.com/acme/app/pull/2", Branch: "feat/second", State: "OPEN", Title: "Second (UL-7)"}},
		Done: []PR{{Number: 1, URL: "https://github.com/acme/app/pull/1", Branch: "feat/first", State: "MERGED", Title: "First (UL-7)"}},
	}
	ticket := Ticket{ID: "UL-7", URL: "https://notion.so/7", Group: TicketDoing, Status: "In dev"}

	b := buildBoard(nil, worktrees, gh, []Ticket{ticket}, nil, time.Now())
	for _, u := range b.Units {
		if slices.Contains(u.Attention, "UL-7 still In dev") {
			t.Errorf("%s: a ticket with an open PR is flagged", u.Branch)
		}
	}

	gh.Open = nil
	b = buildBoard(nil, worktrees[:1], gh, []Ticket{ticket}, nil, time.Now())
	if got := b.Units[0].Attention; !slices.Equal(got, []string{"UL-7 still In dev"}) {
		t.Errorf("all PRs merged: attention = %q", got)
	}
}
