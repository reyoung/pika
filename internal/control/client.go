package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
)

type HTTPError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("daemon returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func Health(ctx context.Context, socketPath string) (protocol.Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://pika-go/v1/health", nil)
	if err != nil {
		return protocol.Health{}, fmt.Errorf("create health request: %w", err)
	}
	resp, err := unixClient(socketPath, 2*time.Second).Do(req)
	if err != nil {
		return protocol.Health{}, fmt.Errorf("contact daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.Health{}, fmt.Errorf("daemon health returned HTTP %d", resp.StatusCode)
	}

	var health protocol.Health
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return protocol.Health{}, fmt.Errorf("decode daemon health: %w", err)
	}
	if err := protocol.CheckVersion(health.ProtocolVersion); err != nil {
		return protocol.Health{}, err
	}
	return health, nil
}

func Status(ctx context.Context, socketPath string) (symphony.View, error) {
	var view symphony.View
	if err := doJSON(ctx, socketPath, http.MethodGet, "/v1/status", nil, &view); err != nil {
		return symphony.View{}, err
	}
	return view, nil
}

func Init(ctx context.Context, socketPath string, request protocol.InitRequest) (symphony.Receipt, error) {
	var receipt symphony.Receipt
	if err := doJSONWithTimeout(ctx, socketPath, http.MethodPost, "/v1/init", request, &receipt, 60*time.Second); err != nil {
		return symphony.Receipt{}, err
	}
	return receipt, nil
}

func InitOptions(ctx context.Context, socketPath string) (protocol.InitOptionsResponse, error) {
	var response protocol.InitOptionsResponse
	// Provider discovery is sequential and Cursor performs separate version,
	// authentication, and model-catalog commands under bounded server budgets.
	if err := doJSONWithTimeout(ctx, socketPath, http.MethodGet, "/v1/init/options", nil, &response, 50*time.Second); err != nil {
		return protocol.InitOptionsResponse{}, err
	}
	return response, nil
}

func DraftBaseline(ctx context.Context, socketPath string, request protocol.DraftBaselineRequest) (symphony.Receipt, error) {
	return mutate(ctx, socketPath, "/v1/baseline-drafts", request)
}

func BackOff(ctx context.Context, socketPath string, request protocol.BackOffRequest) (symphony.Receipt, error) {
	return mutate(ctx, socketPath, "/v1/back-offs", request)
}

func CancelWork(ctx context.Context, socketPath, workID string, request protocol.CancelWorkRequest) (symphony.Receipt, error) {
	return mutate(ctx, socketPath, "/v1/works/"+workID+"/cancel", request)
}

func Shutdown(ctx context.Context, socketPath string, request protocol.ShutdownRequest) (symphony.Receipt, error) {
	return mutate(ctx, socketPath, "/v1/shutdown", request)
}

func PauseScheduler(ctx context.Context, socketPath string, request protocol.SchedulerControlRequest) (protocol.SchedulerControlResponse, error) {
	return controlScheduler(ctx, socketPath, "/v1/scheduler/pause", request)
}

func ResumeScheduler(ctx context.Context, socketPath string, request protocol.SchedulerControlRequest) (protocol.SchedulerControlResponse, error) {
	return controlScheduler(ctx, socketPath, "/v1/scheduler/resume", request)
}

func controlScheduler(ctx context.Context, socketPath, path string, request protocol.SchedulerControlRequest) (protocol.SchedulerControlResponse, error) {
	var response protocol.SchedulerControlResponse
	if err := doJSONWithTimeout(ctx, socketPath, http.MethodPost, path, request, &response, 65*time.Second); err != nil {
		return protocol.SchedulerControlResponse{}, err
	}
	return response, nil
}

func Backup(ctx context.Context, socketPath string, request protocol.BackupRequest) (protocol.BackupResponse, error) {
	var response protocol.BackupResponse
	if err := doJSON(ctx, socketPath, http.MethodPost, "/v1/backups", request, &response); err != nil {
		return protocol.BackupResponse{}, err
	}
	return response, nil
}

func ApplyGitIntent(ctx context.Context, socketPath, intentID string, request protocol.ApplyGitIntentRequest) (protocol.ApplyGitIntentResponse, error) {
	var response protocol.ApplyGitIntentResponse
	if err := doJSON(ctx, socketPath, http.MethodPost, "/v1/git-intents/"+intentID+"/apply", request, &response); err != nil {
		return protocol.ApplyGitIntentResponse{}, err
	}
	return response, nil
}

func IngestProviderEvent(ctx context.Context, socketPath, provider string, request protocol.ProviderEventRequest) error {
	// Provider hooks may carry complete Shell/MCP output and are allowed a larger
	// bounded transfer window than ordinary small control-plane requests.
	return doJSONWithTimeout(ctx, socketPath, http.MethodPost, "/v1/provider-events/"+provider, request, nil, 10*time.Second)
}

func mutate(ctx context.Context, socketPath, path string, input any) (symphony.Receipt, error) {
	var receipt symphony.Receipt
	if err := doJSON(ctx, socketPath, http.MethodPost, path, input, &receipt); err != nil {
		return symphony.Receipt{}, err
	}
	return receipt, nil
}

func IsHTTPStatus(err error, status int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

func doJSON(ctx context.Context, socketPath, method, path string, input, output any) error {
	return doJSONWithTimeout(ctx, socketPath, method, path, input, output, 2*time.Second)
}

func doJSONWithTimeout(ctx context.Context, socketPath, method, path string, input, output any, timeout time.Duration) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode daemon request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://pika-go"+path, body)
	if err != nil {
		return fmt.Errorf("create daemon request: %w", err)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := unixClient(socketPath, timeout).Do(req)
	if err != nil {
		return fmt.Errorf("contact daemon: %w", err)
	}
	defer resp.Body.Close()
	versionText := resp.Header.Get(protocol.VersionHeader)
	if versionText == "" {
		return errors.New("daemon response is missing protocol version")
	}
	var remoteVersion int
	if _, err := fmt.Sscanf(versionText, "%d", &remoteVersion); err != nil {
		return fmt.Errorf("decode daemon protocol version %q: %w", versionText, err)
	}
	if err := protocol.CheckVersion(remoteVersion); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body protocol.ErrorBody
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return &HTTPError{StatusCode: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		}
		return &HTTPError{StatusCode: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
		return fmt.Errorf("decode daemon response: %w", err)
	}
	return nil
}

func unixClient(socketPath string, timeout time.Duration) *http.Client {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}
