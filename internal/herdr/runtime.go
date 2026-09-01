package herdr

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Runtime struct {
	client *Client
}

func NewRuntime(client *Client) *Runtime { return &Runtime{client: client} }

type Agent struct {
	Name             *string `json:"name,omitempty"`
	Agent            *string `json:"agent,omitempty"`
	AgentStatus      string  `json:"agent_status"`
	InteractiveReady bool    `json:"interactive_ready"`
	PaneID           string  `json:"pane_id"`
	TerminalID       string  `json:"terminal_id"`
	WorkspaceID      string  `json:"workspace_id"`
	TabID            string  `json:"tab_id"`
}

type StartSpec struct {
	Name      string
	Kind      string
	PaneID    string
	Arguments []string
	TimeoutMS uint64
	// ReturnOnLaunch accepts the agent.start result without waiting for an idle
	// interactive prompt. Providers with a positional initial prompt can already
	// be executing useful work at that point.
	ReturnOnLaunch bool
}

func (r *Runtime) ReportInstance(ctx context.Context, workspaceID, instanceID string) error {
	if workspaceID == "" || instanceID == "" {
		return errors.New("workspace and instance IDs are required")
	}
	return r.client.Call(ctx, "workspace.report_metadata", map[string]any{
		"workspace_id": workspaceID,
		"source":       "pika-go",
		"tokens":       map[string]string{"pika_instance": instanceID},
	}, nil)
}

func (r *Runtime) SplitPane(ctx context.Context, targetPaneID, direction, cwd string) (Pane, error) {
	if direction != "right" && direction != "down" {
		return Pane{}, fmt.Errorf("unsupported split direction %q", direction)
	}
	params := map[string]any{"target_pane_id": targetPaneID, "direction": direction, "focus": false}
	if cwd != "" {
		params["cwd"] = cwd
	}
	var result struct {
		Pane Pane `json:"pane"`
	}
	if err := r.client.Call(ctx, "pane.split", params, &result); err != nil {
		return Pane{}, err
	}
	return result.Pane, nil
}

func (r *Runtime) CreateTab(ctx context.Context, workspaceID, cwd, label string) (Pane, error) {
	if workspaceID == "" {
		return Pane{}, errors.New("workspace ID is required")
	}
	params := map[string]any{"workspace_id": workspaceID, "focus": false}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if label != "" {
		params["label"] = label
	}
	var result struct {
		RootPane Pane `json:"root_pane"`
	}
	if err := r.client.Call(ctx, "tab.create", params, &result); err != nil {
		return Pane{}, err
	}
	if result.RootPane.PaneID == "" {
		return Pane{}, errors.New("tab.create returned no root pane")
	}
	return result.RootPane, nil
}

func (r *Runtime) Start(ctx context.Context, spec StartSpec) (Agent, error) {
	if spec.Name == "" || spec.Kind == "" || spec.PaneID == "" {
		return Agent{}, errors.New("agent name, kind, and pane ID are required")
	}
	arguments := spec.Arguments
	if arguments == nil {
		arguments = []string{}
	}
	params := map[string]any{"name": spec.Name, "kind": spec.Kind, "pane_id": spec.PaneID, "args": arguments}
	if spec.TimeoutMS != 0 {
		params["timeout_ms"] = spec.TimeoutMS
	}
	var result struct {
		Agent Agent `json:"agent"`
	}
	if err := r.client.Call(ctx, "agent.start", params, &result); err != nil {
		return Agent{}, err
	}
	if spec.ReturnOnLaunch {
		return result.Agent, nil
	}
	timeout := 30 * time.Second
	if spec.TimeoutMS != 0 {
		timeout = time.Duration(spec.TimeoutMS) * time.Millisecond
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		agent, err := r.GetAgent(waitCtx, spec.PaneID)
		if err == nil {
			if agent.AgentStatus == "blocked" {
				// A blocked agent is still a successfully launched process. The
				// orchestrator must be allowed to bind its session and deliver the
				// activation prompt that can make it interactive.
				return agent, nil
			}
			if agent.InteractiveReady {
				return agent, nil
			}
		}
		select {
		case <-waitCtx.Done():
			return Agent{}, fmt.Errorf("wait for agent %s readiness: %w", spec.Name, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (r *Runtime) GetAgent(ctx context.Context, target string) (Agent, error) {
	var result struct {
		Agent Agent `json:"agent"`
	}
	if err := r.client.Call(ctx, "agent.get", map[string]string{"target": target}, &result); err != nil {
		return Agent{}, err
	}
	return result.Agent, nil
}

func (r *Runtime) Prompt(ctx context.Context, target, message string) (Agent, error) {
	if target == "" || message == "" {
		return Agent{}, errors.New("agent target and message are required")
	}
	var result struct {
		Agent Agent `json:"agent"`
	}
	if err := r.client.Call(ctx, "agent.prompt", map[string]any{"target": target, "text": message}, &result); err != nil {
		return Agent{}, err
	}
	return result.Agent, nil
}

func (r *Runtime) GetPane(ctx context.Context, paneID string) (Pane, error) {
	var result struct {
		Pane Pane `json:"pane"`
	}
	if err := r.client.Call(ctx, "pane.get", map[string]string{"pane_id": paneID}, &result); err != nil {
		return Pane{}, err
	}
	return result.Pane, nil
}

// ReadPane returns a bounded terminal transcript for diagnostics. It is not
// a completion protocol; callers use it only to explain a failed observable
// lifecycle transition.
func (r *Runtime) ReadPane(ctx context.Context, paneID string) (string, error) {
	var result struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := r.client.Call(ctx, "pane.read", map[string]any{"pane_id": paneID, "source": "recent_unwrapped", "lines": 120}, &result); err != nil {
		return "", err
	}
	if result.Text != "" {
		return result.Text, nil
	}
	return result.Content, nil
}

func (r *Runtime) RenamePane(ctx context.Context, paneID, label string) error {
	if paneID == "" || label == "" {
		return errors.New("pane ID and label are required")
	}
	return r.client.Call(ctx, "pane.rename", map[string]string{"pane_id": paneID, "label": label}, nil)
}

func (r *Runtime) RenameTab(ctx context.Context, tabID, label string) error {
	if tabID == "" || label == "" {
		return errors.New("tab ID and label are required")
	}
	return r.client.Call(ctx, "tab.rename", map[string]string{"tab_id": tabID, "label": label}, nil)
}

func (r *Runtime) ClosePane(ctx context.Context, paneID string) error {
	if paneID == "" {
		return errors.New("pane ID is required")
	}
	return r.client.Call(ctx, "pane.close", map[string]string{"pane_id": paneID}, nil)
}

func (r *Runtime) SendInput(ctx context.Context, paneID, text string, keys []string) error {
	if paneID == "" || (text == "" && len(keys) == 0) {
		return errors.New("pane ID and input are required")
	}
	return r.client.Call(ctx, "pane.send_input", map[string]any{"pane_id": paneID, "text": text, "keys": keys}, nil)
}

func (r *Runtime) SendKeys(ctx context.Context, paneID string, keys []string) error {
	if paneID == "" || len(keys) == 0 {
		return errors.New("pane ID and keys are required")
	}
	return r.client.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

func (r *Runtime) SendAgentKeys(ctx context.Context, target string, keys []string) error {
	if target == "" || len(keys) == 0 {
		return errors.New("agent target and keys are required")
	}
	return r.client.Call(ctx, "agent.send_keys", map[string]any{"target": target, "keys": keys}, nil)
}
