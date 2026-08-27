package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
)

const MinimumProtocol = 20

type Client struct {
	socketPath string
	nextID     atomic.Uint64
}

func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

type Event struct {
	Kind string          `json:"event"`
	Data json.RawMessage `json:"data"`
}

type Pane struct {
	PaneID        string            `json:"pane_id"`
	TerminalID    string            `json:"terminal_id"`
	WorkspaceID   string            `json:"workspace_id"`
	TabID         string            `json:"tab_id"`
	Agent         *string           `json:"agent,omitempty"`
	AgentStatus   string            `json:"agent_status"`
	Revision      uint64            `json:"revision"`
	Tokens        map[string]string `json:"tokens,omitempty"`
	ForegroundCWD *string           `json:"foreground_cwd,omitempty"`
}

type Workspace struct {
	WorkspaceID string            `json:"workspace_id"`
	Tokens      map[string]string `json:"tokens,omitempty"`
}

type Snapshot struct {
	Version    string            `json:"version"`
	Protocol   uint32            `json:"protocol"`
	Workspaces []Workspace       `json:"workspaces"`
	Panes      []Pane            `json:"panes"`
	Tabs       []json.RawMessage `json:"tabs"`
	Layouts    []json.RawMessage `json:"layouts"`
	Agents     []Agent           `json:"agents"`
}

type Bootstrap struct {
	Snapshot Snapshot
	Buffered []Event
	Stream   *Stream
}

var lifecycleSubscriptions = []string{
	"workspace.created", "workspace.updated", "workspace.metadata_updated", "workspace.closed",
	"tab.created", "tab.closed", "tab.moved",
	"pane.created", "pane.updated", "pane.closed", "pane.moved", "pane.exited", "pane.agent_detected",
}

func (c *Client) Bootstrap(ctx context.Context) (Bootstrap, error) {
	subscriptions := make([]map[string]string, 0, len(lifecycleSubscriptions))
	for _, kind := range lifecycleSubscriptions {
		subscriptions = append(subscriptions, map[string]string{"type": kind})
	}
	stream, err := c.Subscribe(ctx, subscriptions)
	if err != nil {
		return Bootstrap{}, err
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		_ = stream.Close()
		return Bootstrap{}, err
	}
	if snapshot.Protocol < MinimumProtocol {
		_ = stream.Close()
		return Bootstrap{}, fmt.Errorf("Herdr protocol %d is older than required protocol %d", snapshot.Protocol, MinimumProtocol)
	}
	return Bootstrap{Snapshot: snapshot, Buffered: stream.DrainAvailable(), Stream: stream}, nil
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var result struct {
		Snapshot Snapshot `json:"snapshot"`
	}
	if err := c.Call(ctx, "session.snapshot", map[string]any{}, &result); err != nil {
		return Snapshot{}, err
	}
	return result.Snapshot, nil
}

func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if c.socketPath == "" || method == "" {
		return errors.New("Herdr socket path and method are required")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("connect to Herdr: %w", err)
	}
	defer connection.Close()
	id := c.requestID()
	if err := json.NewEncoder(connection).Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("send Herdr request: %w", err)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(connection).Decode(&raw); err != nil {
		return fmt.Errorf("read Herdr response: %w", err)
	}
	var response wireResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode Herdr response: %w", err)
	}
	if response.Error != nil {
		return response.Error
	}
	if response.ID != id {
		return fmt.Errorf("Herdr response ID %q does not match request ID %q: %s", response.ID, id, raw)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode Herdr result for %s: %w", method, err)
	}
	return nil
}

func (c *Client) Subscribe(ctx context.Context, subscriptions []map[string]string) (*Stream, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to Herdr event stream: %w", err)
	}
	id := c.requestID()
	if err := json.NewEncoder(connection).Encode(map[string]any{"id": id, "method": "events.subscribe", "params": map[string]any{"subscriptions": subscriptions}}); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("send Herdr subscription: %w", err)
	}
	reader := bufio.NewReader(connection)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("read Herdr subscription acknowledgement: %w", err)
	}
	var response wireResponse
	if err := json.Unmarshal(line, &response); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("decode Herdr subscription acknowledgement: %w", err)
	}
	if response.ID != id {
		_ = connection.Close()
		return nil, fmt.Errorf("Herdr subscription response ID %q does not match request ID %q", response.ID, id)
	}
	if response.Error != nil {
		_ = connection.Close()
		return nil, response.Error
	}
	stream := &Stream{connection: connection, events: make(chan Event, 256), errors: make(chan error, 1), done: make(chan struct{})}
	go stream.read(reader)
	return stream, nil
}

func (c *Client) requestID() string {
	return fmt.Sprintf("pika-go-%d", c.nextID.Add(1))
}

type wireResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

type Stream struct {
	connection net.Conn
	events     chan Event
	errors     chan error
	done       chan struct{}
	closed     atomic.Bool
}

func (s *Stream) Events() <-chan Event { return s.events }
func (s *Stream) Errors() <-chan error { return s.errors }

func (s *Stream) DrainAvailable() []Event {
	var events []Event
	for {
		select {
		case event, ok := <-s.events:
			if !ok {
				return events
			}
			events = append(events, event)
		default:
			return events
		}
	}
}

func (s *Stream) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(s.done)
	return s.connection.Close()
}

func (s *Stream) read(reader *bufio.Reader) {
	defer close(s.events)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				s.errors <- fmt.Errorf("read Herdr event: %w", err)
				return
			}
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			s.errors <- fmt.Errorf("decode Herdr event: %w", err)
			return
		}
		select {
		case s.events <- event:
		case <-s.done:
			return
		}
	}
}
