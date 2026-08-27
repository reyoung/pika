package control_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/protocol"
)

func TestHealthRejectsIncompatibleDaemonProtocol(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-protocol-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	listener, err := net.Listen("unix", filepath.Join(socketDir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(protocol.Health{Status: "ok", Version: "future", ProtocolVersion: protocol.Version + 1})
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	_, err = control.Health(context.Background(), listener.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "incompatible daemon protocol") {
		t.Fatalf("Health error = %v", err)
	}
}
