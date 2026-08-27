package workruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/herdr"
)

type HerdrRuntime struct {
	Client       *herdr.Client
	Runtime      *herdr.Runtime
	SymphonyPane string
	LaunchDir    string
	OnEvent      func(context.Context, herdr.Event) error
}

func NewHerdrRuntime(client *herdr.Client, symphonyPane string) *HerdrRuntime {
	return &HerdrRuntime{Client: client, Runtime: herdr.NewRuntime(client), SymphonyPane: symphonyPane}
}

func (r *HerdrRuntime) Snapshot(ctx context.Context) (Snapshot, error) {
	if r.Client == nil {
		return Snapshot{}, errors.New("Herdr client is required")
	}
	snapshot, err := r.Client.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFromHerdr(snapshot), nil
}

func snapshotFromHerdr(snapshot herdr.Snapshot) Snapshot {
	byTerminal := make(map[string]Observation, len(snapshot.Panes))
	for _, pane := range snapshot.Panes {
		observation := Observation{
			WorkspaceID: pane.WorkspaceID,
			TabID:       pane.TabID,
			PaneID:      pane.PaneID,
			TerminalID:  pane.TerminalID,
			Status:      pane.AgentStatus,
		}
		if pane.Agent != nil {
			observation.AgentKind = *pane.Agent
		}
		byTerminal[pane.TerminalID] = observation
	}
	for _, agent := range snapshot.Agents {
		observation := byTerminal[agent.TerminalID]
		if observation.PaneID == "" {
			observation.WorkspaceID = agent.WorkspaceID
			observation.TabID = agent.TabID
			observation.PaneID = agent.PaneID
		}
		observation.TerminalID = agent.TerminalID
		observation.Status = agent.AgentStatus
		if agent.Name != nil {
			observation.AgentName = *agent.Name
		}
		if agent.Agent != nil {
			observation.AgentKind = *agent.Agent
		}
		byTerminal[agent.TerminalID] = observation
	}
	result := Snapshot{Sessions: make([]Observation, 0, len(byTerminal))}
	for _, observation := range byTerminal {
		result.Sessions = append(result.Sessions, observation)
	}
	return result
}

func (r *HerdrRuntime) Start(ctx context.Context, spec StartSpec) (Observation, error) {
	if r.Runtime == nil {
		return Observation{}, errors.New("Herdr runtime is required")
	}
	paneID := spec.PreferredPaneID
	if paneID == "" {
		if r.SymphonyPane == "" {
			return Observation{}, errors.New("Symphony pane is required when no preferred pane is supplied")
		}
		pane, err := r.Runtime.SplitPane(ctx, r.SymphonyPane, "down", spec.Repository)
		if err != nil {
			return Observation{}, err
		}
		paneID = pane.PaneID
	}
	cleanup, err := r.prepareEnvironment(ctx, paneID, spec.Environment)
	if err != nil {
		return Observation{}, err
	}
	defer cleanup()
	agent, err := r.Runtime.Start(ctx, herdr.StartSpec{Name: spec.AgentName, Kind: spec.AgentKind, PaneID: paneID})
	if err != nil {
		return Observation{}, err
	}
	return observationFromAgent(agent), nil
}

var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func (r *HerdrRuntime) prepareEnvironment(ctx context.Context, paneID string, values map[string]string) (func(), error) {
	if len(values) == 0 {
		return func() {}, nil
	}
	if r.LaunchDir == "" || !filepath.IsAbs(r.LaunchDir) {
		return nil, errors.New("absolute launch directory is required for agent environment")
	}
	if err := os.MkdirAll(r.LaunchDir, 0o700); err != nil {
		return nil, fmt.Errorf("create launch directory: %w", err)
	}
	file, err := os.CreateTemp(r.LaunchDir, ".agent-env-*.sh")
	if err != nil {
		return nil, fmt.Errorf("create agent environment: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return nil, fmt.Errorf("protect agent environment: %w", err)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if !environmentName.MatchString(key) {
			_ = file.Close()
			cleanup()
			return nil, fmt.Errorf("invalid agent environment name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(file, "export %s=%s\n", key, shellQuote(values[key])); err != nil {
			_ = file.Close()
			cleanup()
			return nil, fmt.Errorf("write agent environment: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("close agent environment: %w", err)
	}
	command := ". " + shellQuote(path) + " && rm -f " + shellQuote(path)
	if err := r.Runtime.SendInput(ctx, paneID, command, []string{"enter"}); err != nil {
		cleanup()
		return nil, fmt.Errorf("load agent environment: %w", err)
	}
	// pane.send_input acknowledges delivery, not shell execution. The command removes
	// the file only after sourcing it, which gives agent.start a concrete readiness gate.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		} else if err != nil {
			cleanup()
			return nil, fmt.Errorf("observe agent environment consumption: %w", err)
		}
		select {
		case <-ctx.Done():
			cleanup()
			return nil, fmt.Errorf("wait for shell to load agent environment: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (r *HerdrRuntime) Prompt(ctx context.Context, target, message string) error {
	if r.Runtime == nil {
		return errors.New("Herdr runtime is required")
	}
	_, err := r.Runtime.Prompt(ctx, target, message)
	return err
}

func (r *HerdrRuntime) Close(ctx context.Context, paneID string) error {
	if r.Runtime == nil {
		return errors.New("Herdr runtime is required")
	}
	err := r.Runtime.ClosePane(ctx, paneID)
	var apiErr *herdr.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "pane_not_found" {
		return nil
	}
	return err
}

func (r *HerdrRuntime) Watch(ctx context.Context, onChange func(context.Context, Snapshot) error) error {
	if r.Client == nil || onChange == nil {
		return errors.New("Herdr client and change callback are required")
	}
	bootstrap, err := r.Client.Bootstrap(ctx)
	if err != nil {
		return err
	}
	defer bootstrap.Stream.Close()
	if err := onChange(ctx, snapshotFromHerdr(bootstrap.Snapshot)); err != nil {
		return err
	}
	if len(bootstrap.Buffered) != 0 {
		snapshot, err := r.Snapshot(ctx)
		if err != nil {
			return err
		}
		if err := onChange(ctx, snapshot); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-bootstrap.Stream.Events():
			if !ok {
				return errors.New("Herdr event stream closed")
			}
			if r.OnEvent != nil {
				if err := r.OnEvent(ctx, event); err != nil {
					return err
				}
			}
			snapshot, err := r.Snapshot(ctx)
			if err != nil {
				return err
			}
			if err := onChange(ctx, snapshot); err != nil {
				return err
			}
		case err := <-bootstrap.Stream.Errors():
			if err == nil {
				return errors.New("Herdr event stream failed")
			}
			return err
		}
	}
}

func observationFromAgent(agent herdr.Agent) Observation {
	observation := Observation{
		WorkspaceID: agent.WorkspaceID,
		TabID:       agent.TabID,
		PaneID:      agent.PaneID,
		TerminalID:  agent.TerminalID,
		Status:      agent.AgentStatus,
	}
	if agent.Name != nil {
		observation.AgentName = *agent.Name
	}
	if agent.Agent != nil {
		observation.AgentKind = *agent.Agent
	}
	return observation
}
