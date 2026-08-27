package herdr_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/herdr"
)

func TestBootstrapSubscribesBeforeSnapshotAndBuffersGapEvents(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("/tmp", "pika-go-herdr-client-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "herdr.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() { serverDone <- serveBootstrapFixture(listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bootstrap, err := herdr.NewClient(listener.Addr().String()).Bootstrap(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = bootstrap.Stream.Close() })
	if bootstrap.Snapshot.Protocol != 20 || bootstrap.Snapshot.Version != "0.8.2" || len(bootstrap.Snapshot.Panes) != 1 || bootstrap.Snapshot.Panes[0].PaneID != "w1:p1" {
		t.Fatalf("snapshot = %+v", bootstrap.Snapshot)
	}
	if len(bootstrap.Buffered) != 1 || bootstrap.Buffered[0].Kind != "pane.created" {
		t.Fatalf("buffered events = %+v", bootstrap.Buffered)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("fixture server: %v", err)
	}
}

func serveBootstrapFixture(listener net.Listener) error {
	subscription, err := listener.Accept()
	if err != nil {
		return err
	}
	defer subscription.Close()
	subReader := bufio.NewReader(subscription)
	requestLine, err := subReader.ReadBytes('\n')
	if err != nil {
		return err
	}
	var subscribeRequest struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(requestLine, &subscribeRequest); err != nil {
		return err
	}
	if subscribeRequest.Method != "events.subscribe" {
		return &fixtureError{"first method", subscribeRequest.Method}
	}
	if _, err := subscription.Write([]byte(`{"id":"` + subscribeRequest.ID + `","result":{"type":"subscribed"}}` + "\n")); err != nil {
		return err
	}
	if _, err := subscription.Write([]byte(`{"event":"pane.created","data":{"pane":{"pane_id":"w1:p2"}}}` + "\n")); err != nil {
		return err
	}

	snapshot, err := listener.Accept()
	if err != nil {
		return err
	}
	defer snapshot.Close()
	snapshotLine, err := bufio.NewReader(snapshot).ReadBytes('\n')
	if err != nil {
		return err
	}
	var snapshotRequest struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(snapshotLine, &snapshotRequest); err != nil {
		return err
	}
	if snapshotRequest.Method != "session.snapshot" {
		return &fixtureError{"second method", snapshotRequest.Method}
	}
	_, err = snapshot.Write([]byte(`{"id":"` + snapshotRequest.ID + `","result":{"type":"session_snapshot","snapshot":{"version":"0.8.2","protocol":20,"workspaces":[],"tabs":[],"panes":[{"pane_id":"w1:p1","terminal_id":"term-1","workspace_id":"w1","tab_id":"w1:t1","focused":true,"agent_status":"unknown","revision":0}],"layouts":[],"agents":[]}}}` + "\n"))
	return err
}

type fixtureError struct {
	label string
	got   string
}

func (e *fixtureError) Error() string { return e.label + ": got " + e.got }
