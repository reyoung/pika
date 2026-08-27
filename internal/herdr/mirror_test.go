package herdr_test

import (
	"encoding/json"
	"testing"

	"github.com/reyoung/pika-go/internal/herdr"
)

func TestMirrorTracksPaneMoveByStableTerminalIdentity(t *testing.T) {
	t.Parallel()

	mirror := herdr.NewMirror(herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p2", TerminalID: "term-1", WorkspaceID: "w1", TabID: "w1:t1"}}})
	data, _ := json.Marshal(map[string]any{
		"type":             "pane_moved",
		"previous_pane_id": "w1:p2",
		"pane":             herdr.Pane{PaneID: "w2:p1", TerminalID: "term-1", WorkspaceID: "w2", TabID: "w2:t1"},
	})
	mirror.Apply(herdr.Event{Kind: "pane.moved", Data: data})
	pane, ok := mirror.PaneByTerminal("term-1")
	if !ok || pane.PaneID != "w2:p1" || pane.WorkspaceID != "w2" {
		t.Fatalf("moved pane = %+v, found = %v", pane, ok)
	}
}
