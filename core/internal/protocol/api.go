package protocol

import "time"

// SessionParams are the optional create-time parameters (C1). Core ignores
// anything else; `workspace` accepts only "none" in phase 1.
type SessionParams struct {
	Tier      string `json:"tier,omitempty"` // fast | smart | deep (deep: admin only)
	Effort    string `json:"effort,omitempty"`
	Incognito *bool  `json:"incognito,omitempty"`
	Workspace string `json:"workspace,omitempty"` // "none"
}

// CreateSession is the body of POST /v1/sessions. There is no `source`: Core
// derives it from the auth path.
type CreateSession struct {
	Agent  string        `json:"agent"`
	Params SessionParams `json:"params"`
}

// Session is the client's view of a session row.
type Session struct {
	ID              string     `json:"id"`
	RootID          string     `json:"root_id"`
	ParentID        *string    `json:"parent_id"`
	ForkAtMessageID *string    `json:"fork_at_message_id"`
	AgentID         string     `json:"agent_id"`
	AgentVersion    string     `json:"agent_version"`
	Title           string     `json:"title"`
	Tier            string     `json:"tier"`
	Model           string     `json:"model"`
	Effort          string     `json:"effort"`
	Incognito       bool       `json:"incognito"`
	Workspace       string     `json:"workspace"` // sandbox: "none" in phase 1
	Source          string     `json:"source"`    // web | ci
	Status          string     `json:"status"`    // running | idle | requires_action | error
	Tainted         bool       `json:"tainted"`
	ArchivedAt      *time.Time `json:"archived_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastActivityAt  time.Time  `json:"last_activity_at"`
}

// SessionList is the result of GET /v1/sessions.
type SessionList struct {
	Sessions []Session `json:"sessions"`
	Next     string    `json:"next,omitempty"`
}

// SessionPatch is the body of PATCH /v1/sessions/{id}.
type SessionPatch struct {
	Title string `json:"title"`
}

// ForkRequest is the body of POST /v1/sessions/{id}/fork.
type ForkRequest struct {
	AtMessage string `json:"at_message"`
}

// SendResult is the result of POST /v1/sessions/{id}/events.
type SendResult struct {
	Message *Message `json:"message,omitempty"`
}

// TreeMessage is one node of a Tree.
type TreeMessage struct {
	Message   Message `json:"message"`
	ParentID  *string `json:"parent_id"`
	SessionID string  `json:"session_id"`
}

// Tree is the result of GET /v1/sessions/{root}/tree.
type Tree struct {
	Messages      []TreeMessage    `json:"messages"`
	HeadID        *string          `json:"head_id"`
	LastEventIDs  map[string]int64 `json:"last_event_ids"`
	PendingWrites []MemoryWrite    `json:"pending_writes"`
}

// Attachment is the result of POST /v1/attachments.
type Attachment struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Pages     int    `json:"pages,omitempty"`
}

// Profile is the person's memory profile.
type Profile struct {
	Content   string        `json:"content"`
	Version   int           `json:"version"`
	UpdatedAt time.Time     `json:"updated_at"`
	Pending   []MemoryWrite `json:"pending"`
}

// ProfileUpdate is the body of PUT /v1/memory/profile.
type ProfileUpdate struct {
	Content string `json:"content"`
	Version int    `json:"version"`
}

// BudgetStatus is the month's spend against the global cap.
type BudgetStatus struct {
	SpentUSD  float64   `json:"spent_usd"`
	CapUSD    float64   `json:"cap_usd"`
	Ratio     float64   `json:"ratio"`
	Warn      bool      `json:"warn"`
	Exhausted bool      `json:"exhausted"`
	ResetsAt  time.Time `json:"resets_at"`
}

// MePerson is the person block of Me.
type MePerson struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Tier        string `json:"tier"` // admin | family
}

// Me is the result of GET /v1/me.
type Me struct {
	Person      MePerson     `json:"person"`
	Budget      BudgetStatus `json:"budget"`
	Maintenance bool         `json:"maintenance"`
}

// AgentSummary is the result of GET /v1/agents/{id}, and an element of the list.
type AgentSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tiers       []string `json:"tiers"`
	DefaultTier string   `json:"default_tier"`
}

// AgentList is the result of GET /v1/agents.
type AgentList struct {
	Agents []AgentSummary `json:"agents"`
}
