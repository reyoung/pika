package instance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/reyoung/pika-go/internal/herdr"
)

func ResolveSocket(explicit string) (string, error) {
	if explicit != "" {
		return validateSocketPath(explicit)
	}
	if configured := os.Getenv("PIKA_GO_SOCKET"); configured != "" {
		return validateSocketPath(configured)
	}

	herdrSocket := os.Getenv("HERDR_SOCKET_PATH")
	paneID := os.Getenv("HERDR_PANE_ID")
	workspaceID, _, ok := strings.Cut(paneID, ":")
	if herdrSocket == "" || !ok || workspaceID == "" {
		return "", errors.New("socket is undiscoverable: pass --socket, set PIKA_GO_SOCKET, or run inside a Herdr pane")
	}

	digest := sha256.Sum256([]byte(herdrSocket + "\x00" + workspaceID))
	return SocketForInstance(hex.EncodeToString(digest[:6]))
}

// ResolveSocketContext first asks Herdr for the current Workspace's published
// Pika instance. The deterministic derivation remains only as the daemon's
// first-start bootstrap and as a compatibility fallback when Herdr cannot yet
// answer the metadata query.
func ResolveSocketContext(ctx context.Context, explicit string) (string, error) {
	if explicit != "" || os.Getenv("PIKA_GO_SOCKET") != "" {
		return ResolveSocket(explicit)
	}
	herdrSocket := os.Getenv("HERDR_SOCKET_PATH")
	paneID := os.Getenv("HERDR_PANE_ID")
	workspaceID, _, ok := strings.Cut(paneID, ":")
	if herdrSocket == "" || !ok || workspaceID == "" {
		return ResolveSocket(explicit)
	}
	var result struct {
		Workspace herdr.Workspace `json:"workspace"`
	}
	if err := herdr.NewClient(herdrSocket).Call(ctx, "workspace.get", map[string]string{"workspace_id": workspaceID}, &result); err == nil {
		if instanceID := result.Workspace.Tokens["pika_instance"]; instanceID != "" {
			return SocketForInstance(instanceID)
		}
	}
	return ResolveSocket(explicit)
}

func SocketForInstance(instanceID string) (string, error) {
	if err := ValidateID(instanceID); err != nil {
		return "", err
	}
	return filepath.Join("/tmp", "pika-go-"+strconv.Itoa(os.Getuid()), instanceID+".sock"), nil
}

func validateSocketPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("socket path must be absolute: %s", path)
	}
	return filepath.Clean(path), nil
}
