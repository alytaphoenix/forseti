// Package herdrd is a minimal client for the herdr JSON socket API.
//
// Wire facts (verified 2026-09-30, spikes S6/S7 in docs/spikes.md):
//   - NDJSON over a Unix socket, no handshake for the JSON API.
//   - One-shot requests: the server CLOSES the connection after the response,
//     so Call() dials per request.
//   - events.subscribe is the only persistent connection: ack
//     {"result":{"type":"subscription_started"}}, then pushed lines
//     {"event":"<name>","data":{...}} with no id.
//   - events.wait supports only pane_agent_status_changed matches (0.9.3).
//   - agent.read nests payload under result.read.text.
package herdrd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SocketPath resolves the herdr socket the same order the CLI documents:
// HERDR_SOCKET_PATH → HERDR_SESSION named socket → default session socket.
func SocketPath() (string, error) {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p, nil
	}
	base := os.Getenv("HERDR_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config", "herdr")
	}
	if name := os.Getenv("HERDR_SESSION"); name != "" {
		return filepath.Join(base, "sessions", name, "herdr.sock"), nil
	}
	return filepath.Join(base, "herdr.sock"), nil
}

// SocketPathFor resolves the socket for a named session (or the default
// session when name == "").
func SocketPathFor(name string) (string, error) {
	base := os.Getenv("HERDR_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config", "herdr")
	}
	if name == "" {
		return filepath.Join(base, "herdr.sock"), nil
	}
	return filepath.Join(base, "sessions", name, "herdr.sock"), nil
}

// Envelope is the raw request frame.
type Envelope struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Reply is the raw response frame (exactly one of Result/Error).
type Reply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

// APIError is the server error shape.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Client dials the herdr socket. Safe for concurrent use.
type Client struct {
	sock string
	mu   sync.Mutex
	seq  int
}

func New() (*Client, error) {
	p, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return NewAt(p)
}

// NewAt creates a client for an explicit socket path (session bootstrap
// targets the default socket while HERDR_SESSION is overridden).
func NewAt(sock string) (*Client, error) {
	if _, err := os.Stat(sock); err != nil {
		return nil, fmt.Errorf("herdr socket %s not present (server running?): %w", sock, err)
	}
	return &Client{sock: sock}, nil
}

func (c *Client) nextID(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("crew-%s-%d", prefix, c.seq)
}

// Call performs a one-shot request on a fresh connection (the server closes
// the socket after each one-shot response — verified S6).
func (c *Client) Call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	pj, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", c.sock, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	env := Envelope{ID: c.nextID(method), Method: method, Params: pj}
	line, _ := json.Marshal(env)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	reply, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, reply.Error
	}
	return reply.Result, nil
}

func readFrame(conn net.Conn) (*Reply, error) {
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var rep Reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return nil, fmt.Errorf("bad response frame %q: %w", line, err)
	}
	return &rep, nil
}

// Event is a pushed subscription line: {"event":"<name>","data":{...}}.
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// Subscription is a persistent events.subscribe connection.
type Subscription struct {
	conn   net.Conn
	Events chan Event
	Err    error
	closed chan struct{}
	once   sync.Once
}

// Subscribe opens a persistent subscription. The returned Subscription streams
// matching events; on server-side events_lost the Err field is set and Events
// closes. Callers should resubscribe + reconcile via authoritative reads.
func (c *Client) Subscribe(subscriptions []map[string]any, timeout time.Duration) (*Subscription, error) {
	params := map[string]any{"subscriptions": subscriptions}
	pj, _ := json.Marshal(params)
	conn, err := net.DialTimeout("unix", c.sock, timeout)
	if err != nil {
		return nil, err
	}
	env := Envelope{ID: c.nextID("subscribe"), Method: "events.subscribe", Params: pj}
	line, _ := json.Marshal(env)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	rep, err := readFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("subscribe ack: %w", err)
	}
	if rep.Error != nil {
		conn.Close()
		return nil, rep.Error
	}
	var ack struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(rep.Result, &ack)
	if ack.Type != "subscription_started" {
		conn.Close()
		return nil, fmt.Errorf("unexpected subscribe ack: %s", rep.Result)
	}
	sub := &Subscription{conn: conn, Events: make(chan Event, 256), closed: make(chan struct{})}
	go sub.pump()
	return sub, nil
}

func (s *Subscription) pump() {
	defer close(s.Events)
	r := bufio.NewReader(s.conn)
	for {
		select {
		case <-s.closed:
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		line, err := r.ReadBytes('\n')
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue // idle keep-around
			}
			s.Err = err
			return
		}
		if len(line) == 0 {
			continue
		}
		// A second error frame with the original request id means events_lost etc.
		var probe struct {
			ID    string    `json:"id"`
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Error != nil {
			s.Err = probe.Error
			return
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		select {
		case s.Events <- ev:
		case <-s.closed:
			return
		}
	}
}

func (s *Subscription) Close() {
	s.once.Do(func() {
		close(s.closed)
		s.conn.Close()
	})
}

// ---- typed helpers ----

// AgentInfo is the subset of an agent record crew cares about.
type AgentInfo struct {
	Name        string `json:"name"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

func (c *Client) Ping() error {
	_, err := c.Call("ping", struct{}{}, 5*time.Second)
	return err
}

func (c *Client) AgentList() ([]AgentInfo, error) {
	res, err := c.Call("agent.list", struct{}{}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var out struct {
		Agents []AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// AgentRead returns the pane text for the agent.
func (c *Client) AgentRead(target, source string, lines int) (string, error) {
	res, err := c.Call("agent.read", map[string]any{
		"target": target, "source": source, "lines": lines,
	}, 15*time.Second)
	if err != nil {
		return "", err
	}
	var out struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.Read.Text, nil
}

// AgentStart starts a named agent in an existing pane.
func (c *Client) AgentStart(name, kind, paneID string, args []string, timeout time.Duration) error {
	params := map[string]any{"name": name, "kind": kind, "pane_id": paneID}
	if len(args) > 0 {
		params["args"] = args
	}
	_, err := c.Call("agent.start", params, timeout)
	return err
}

// AgentPrompt sends text to the agent.
func (c *Client) AgentPrompt(name, text string) error {
	_, err := c.Call("agent.prompt", map[string]any{"target": name, "text": text}, 15*time.Second)
	return err
}

// AgentPromptWait submits the prompt AND starts the settle wait in ONE request
// (docs: "avoiding a race between separate calls" — a bare wait-idle after a
// prompt can match the pre-prompt idle state; hit live 2026-09-30).
// Returns the settled agent status.
func (c *Client) AgentPromptWait(name, text string, until []string, timeout time.Duration) (string, error) {
	res, err := c.Call("agent.prompt", map[string]any{
		"target": name, "text": text,
		"wait": map[string]any{"until": until, "timeout_ms": timeout.Milliseconds()},
	}, timeout+15*time.Second)
	if err != nil {
		// agent_blocked comes back as an error code
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "agent_blocked" {
			return "blocked", nil
		}
		return "", err
	}
	// response is typically wait_matched carrying the agent status event
	var out struct {
		Type  string `json:"type"`
		Event struct {
			Event string `json:"event"`
			Data  struct {
				AgentStatus string `json:"agent_status"`
			} `json:"data"`
		} `json:"event"`
	}
	_ = json.Unmarshal(res, &out)
	if st := out.Event.Data.AgentStatus; st != "" {
		return st, nil
	}
	if out.Type == "wait_matched" {
		return "idle", nil
	}
	return "idle", nil
}

// AgentFocus jumps the herdr UI to the pane hosting the named agent.
func (c *Client) AgentFocus(name string) error {
	_, err := c.Call("agent.focus", map[string]any{"target": name}, 10*time.Second)
	return err
}

// AgentWait blocks until the agent reaches one of the until states.
func (c *Client) AgentWait(name string, until []string, timeout time.Duration) error {
	_, err := c.Call("agent.wait", map[string]any{
		"target": name, "until": until, "timeout_ms": timeout.Milliseconds(),
	}, timeout+5*time.Second)
	return err
}

// FocusedWorkspace returns the currently focused workspace id.
// (Live shape 2026-09-30: result.snapshot.focused_workspace_id)
func (c *Client) FocusedWorkspace() (string, error) {
	res, err := c.Call("session.snapshot", struct{}{}, 10*time.Second)
	if err != nil {
		return "", err
	}
	var out struct {
		Snapshot struct {
			FocusedWorkspaceID string `json:"focused_workspace_id"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	if out.Snapshot.FocusedWorkspaceID == "" {
		return "", errors.New("snapshot has no focused workspace")
	}
	return out.Snapshot.FocusedWorkspaceID, nil
}

// NotificationShow raises a herdr notification (sound: none|done|request).
func (c *Client) NotificationShow(title, body, sound string) error {
	_, err := c.Call("notification.show", map[string]any{
		"title": title, "body": body, "sound": sound,
	}, 10*time.Second)
	return err
}

// AgentViewSet projects a source-owned view into herdr's Agents sidebar
// (UI-only, S10). filter/sort are raw API shapes.
func (c *Client) AgentViewSet(source, label string, filter map[string]any, sort []map[string]any) error {
	params := map[string]any{"source": source}
	if label != "" {
		params["label"] = label
	}
	if filter != nil {
		params["filter"] = filter
	}
	if len(sort) > 0 {
		params["sort"] = sort
	}
	_, err := c.Call("agent.view.set", params, 10*time.Second)
	return err
}

// AgentViewClear removes this source's projection.
func (c *Client) AgentViewClear(source string) error {
	_, err := c.Call("agent.view.clear", map[string]any{"source": source}, 10*time.Second)
	return err
}

// PaneWaitForOutput blocks until the pane's output matches (S12: matches
// existing scrollback instantly; error codes: timeout | invalid_regex).
// Returns the matched line.
func (c *Client) PaneWaitForOutput(paneID, matchType, value string, timeout time.Duration) (string, error) {
	res, err := c.Call("pane.wait_for_output", map[string]any{
		"pane_id":    paneID,
		"source":     "recent",
		"strip_ansi": true,
		"match":      map[string]any{"type": matchType, "value": value},
		"timeout_ms": timeout.Milliseconds(),
	}, timeout+10*time.Second)
	if err != nil {
		return "", err
	}
	var out struct {
		MatchedLine string `json:"matched_line"`
	}
	_ = json.Unmarshal(res, &out)
	return out.MatchedLine, nil
}

// WorktreeCreate creates a git worktree + dedicated herdr workspace (S13).
// Returns (workspaceID, checkoutPath).
func (c *Client) WorktreeCreate(cwd, branch, path, label string, timeout time.Duration) (string, string, error) {
	params := map[string]any{"cwd": cwd, "trust_repository": true, "focus": false}
	if branch != "" {
		params["branch"] = branch
	}
	if path != "" {
		params["path"] = path
	}
	if label != "" {
		params["label"] = label
	}
	res, err := c.Call("worktree.create", params, timeout)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
			Worktree    struct {
				CheckoutPath string `json:"checkout_path"`
			} `json:"worktree"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", "", err
	}
	return out.Workspace.WorkspaceID, out.Workspace.Worktree.CheckoutPath, nil
}

// WorktreeRemove removes a worktree + its workspace.
func (c *Client) WorktreeRemove(workspaceID string, force bool) error {
	_, err := c.Call("worktree.remove", map[string]any{
		"workspace_id": workspaceID, "force": force, "trust_repository": true,
	}, 60*time.Second)
	return err
}
