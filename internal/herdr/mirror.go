package herdr

import (
	"encoding/json"
	"sync"
)

type Mirror struct {
	mu              sync.RWMutex
	panesByTerminal map[string]Pane
	terminalByPane  map[string]string
}

func NewMirror(snapshot Snapshot) *Mirror {
	mirror := &Mirror{panesByTerminal: map[string]Pane{}, terminalByPane: map[string]string{}}
	for _, pane := range snapshot.Panes {
		mirror.put(pane)
	}
	return mirror
}

func (m *Mirror) Apply(event Event) {
	var data struct {
		Type           string `json:"type"`
		Pane           *Pane  `json:"pane"`
		PaneID         string `json:"pane_id"`
		PreviousPaneID string `json:"previous_pane_id"`
	}
	if json.Unmarshal(event.Data, &data) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if data.PreviousPaneID != "" {
		delete(m.terminalByPane, data.PreviousPaneID)
	}
	if data.Pane != nil {
		m.putLocked(*data.Pane)
		return
	}
	if data.Type == "pane_closed" || data.Type == "pane_exited" {
		if terminalID := m.terminalByPane[data.PaneID]; terminalID != "" {
			delete(m.terminalByPane, data.PaneID)
			delete(m.panesByTerminal, terminalID)
		}
	}
}

func (m *Mirror) PaneByTerminal(terminalID string) (Pane, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pane, ok := m.panesByTerminal[terminalID]
	return pane, ok
}

func (m *Mirror) put(pane Pane) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putLocked(pane)
}

func (m *Mirror) putLocked(pane Pane) {
	if existing, ok := m.panesByTerminal[pane.TerminalID]; ok && existing.PaneID != pane.PaneID {
		delete(m.terminalByPane, existing.PaneID)
	}
	m.panesByTerminal[pane.TerminalID] = pane
	m.terminalByPane[pane.PaneID] = pane.TerminalID
}
