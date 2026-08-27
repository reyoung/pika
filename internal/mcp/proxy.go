package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
)

func RunProxy(ctx context.Context, socketPath, token string, input io.Reader, output io.Writer) error {
	if socketPath == "" || token == "" {
		return fmt.Errorf("daemon socket and MCP grant are required")
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pika-go/mcp", bytes.NewReader(line))
		if err != nil {
			return fmt.Errorf("create MCP forward request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("forward MCP request: %w", err)
		}
		if response.StatusCode != http.StatusNoContent {
			if _, err := io.Copy(output, response.Body); err != nil {
				_ = response.Body.Close()
				return fmt.Errorf("write MCP response: %w", err)
			}
		}
		if err := response.Body.Close(); err != nil {
			return fmt.Errorf("close MCP response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}
