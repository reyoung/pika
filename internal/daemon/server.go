package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
)

type Config struct {
	SocketPath              string
	Version                 string
	InstanceID              string
	Symphony                symphony.Symphony
	PrepareInit             func(context.Context, string, *string) (PreparedInit, error)
	InitOptions             func(context.Context) (protocol.InitOptionsResponse, error)
	RecordInitFailure       func(context.Context, string) error
	AfterListen             func(context.Context) error
	AfterCommit             func(context.Context)
	MCPHandler              http.Handler
	ApplyGitIntent          func(context.Context, string, string) (string, error)
	IngestProviderEvent     func(context.Context, string, string, json.RawMessage) error
	IngestProviderHookEvent func(context.Context, string, string, string, json.RawMessage) error
	Backup                  func(context.Context, string) error
	DrainReady              func(context.Context) (bool, error)
	SchedulerControl        func(context.Context, symphony.Command) (protocol.SchedulerControlResponse, error)
}

type PreparedInit struct {
	Rollback             func() error
	IterationConcurrency int64
	MaxPendingAttempts   int64
}

func Serve(ctx context.Context, cfg Config) error {
	if cfg.SocketPath == "" {
		return errors.New("socket path is required")
	}
	if cfg.Version == "" {
		return errors.New("version is required")
	}

	if err := prepareSocket(cfg.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on unix socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(cfg.SocketPath)
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		return fmt.Errorf("set socket permissions: %w", err)
	}

	mux := http.NewServeMux()
	if cfg.MCPHandler != nil {
		mux.Handle("POST /mcp", cfg.MCPHandler)
	}
	initFailures := make(chan error, 1)
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.Health{
			Status:          "ok",
			Version:         cfg.Version,
			ProtocolVersion: protocol.Version,
		})
	})
	if cfg.Symphony != nil {
		if cfg.InitOptions != nil {
			mux.HandleFunc("GET /v1/init/options", func(w http.ResponseWriter, request *http.Request) {
				options, err := cfg.InitOptions(request.Context())
				if err != nil {
					writeAPIError(w, http.StatusInternalServerError, "init_options_failed", err.Error())
					return
				}
				writeJSON(w, http.StatusOK, options)
			})
		}
		mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, request *http.Request) {
			view, err := cfg.Symphony.Inspect(request.Context(), symphony.Status{})
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, view)
		})
		mux.HandleFunc("POST /v1/init", func(w http.ResponseWriter, request *http.Request) {
			var input protocol.InitRequest
			decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 1<<20))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("decode request: %v", err))
				return
			}
			prepared := PreparedInit{Rollback: func() error { return nil }}
			if cfg.PrepareInit != nil {
				var err error
				prepared, err = cfg.PrepareInit(request.Context(), input.Repository, input.ConfigurationTOML)
				if err != nil {
					message := err.Error()
					if isNotInitialized(request.Context(), cfg.Symphony) {
						if cfg.RecordInitFailure != nil {
							if recordErr := cfg.RecordInitFailure(request.Context(), message); recordErr != nil {
								message = fmt.Sprintf("%s; record diagnostic: %v", message, recordErr)
							}
						}
						defer func() { initFailures <- errors.New(message) }()
					}
					writeAPIError(w, http.StatusBadRequest, "invalid_configuration", message)
					return
				}
			}
			receipt, err := cfg.Symphony.Apply(request.Context(), symphony.Init{
				Meta:                 symphony.CommandMeta{RequestID: input.RequestID, ExpectedRevision: input.ExpectedRevision},
				OptimizationID:       cfg.InstanceID,
				Repository:           input.Repository,
				CallerPaneID:         input.CallerPaneID,
				IterationConcurrency: prepared.IterationConcurrency,
				MaxPendingAttempts:   prepared.MaxPendingAttempts,
			})
			if err != nil {
				if rollbackErr := prepared.Rollback(); rollbackErr != nil {
					writeAPIError(w, http.StatusInternalServerError, "init_rollback_failed", fmt.Sprintf("%v; rollback: %v", err, rollbackErr))
					return
				}
				writeError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, receipt)
			if cfg.AfterCommit != nil {
				cfg.AfterCommit(ctx)
			}
		})
		mux.HandleFunc("POST /v1/baseline-drafts", func(w http.ResponseWriter, request *http.Request) {
			var input protocol.DraftBaselineRequest
			if !decodeJSON(w, request, &input) {
				return
			}
			applyAndWrite(w, request, cfg.Symphony, cfg.AfterCommit, symphony.StartBaselineDraft{Meta: commandMeta(input.Mutation)})
		})
		mux.HandleFunc("POST /v1/back-offs", func(w http.ResponseWriter, request *http.Request) {
			var input protocol.BackOffRequest
			if !decodeJSON(w, request, &input) {
				return
			}
			applyAndWrite(w, request, cfg.Symphony, cfg.AfterCommit, symphony.BackOff{Meta: commandMeta(input.Mutation), WorkID: input.WorkID, Message: input.Message})
		})
		mux.HandleFunc("POST /v1/works/{work_id}/cancel", func(w http.ResponseWriter, request *http.Request) {
			var input protocol.CancelWorkRequest
			if !decodeJSON(w, request, &input) {
				return
			}
			applyAndWrite(w, request, cfg.Symphony, cfg.AfterCommit, symphony.CancelWork{Meta: commandMeta(input.Mutation), WorkID: request.PathValue("work_id")})
		})
		mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, request *http.Request) {
			var input protocol.ShutdownRequest
			if !decodeJSON(w, request, &input) {
				return
			}
			applyAndWrite(w, request, cfg.Symphony, cfg.AfterCommit, symphony.RequestShutdown{Meta: commandMeta(input.Mutation)})
		})
		if cfg.SchedulerControl != nil {
			mux.HandleFunc("POST /v1/scheduler/pause", func(w http.ResponseWriter, request *http.Request) {
				var input protocol.SchedulerControlRequest
				if !decodeJSON(w, request, &input) {
					return
				}
				controlCtx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
				defer cancel()
				response, err := cfg.SchedulerControl(controlCtx, symphony.PauseScheduler{Meta: commandMeta(input.Mutation)})
				if err != nil {
					writeError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, response)
			})
			mux.HandleFunc("POST /v1/scheduler/resume", func(w http.ResponseWriter, request *http.Request) {
				var input protocol.SchedulerControlRequest
				if !decodeJSON(w, request, &input) {
					return
				}
				controlCtx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
				defer cancel()
				response, err := cfg.SchedulerControl(controlCtx, symphony.ResumeScheduler{Meta: commandMeta(input.Mutation)})
				if err != nil {
					writeError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, response)
			})
		}
		if cfg.Backup != nil {
			mux.HandleFunc("POST /v1/backups", func(w http.ResponseWriter, request *http.Request) {
				var input protocol.BackupRequest
				if !decodeJSON(w, request, &input) {
					return
				}
				if input.Destination == "" || !filepath.IsAbs(input.Destination) {
					writeAPIError(w, http.StatusBadRequest, "invalid_request", "backup destination must be an absolute path")
					return
				}
				if err := cfg.Backup(request.Context(), input.Destination); err != nil {
					writeAPIError(w, http.StatusBadRequest, "backup_failed", err.Error())
					return
				}
				info, err := os.Stat(input.Destination)
				if err != nil {
					writeAPIError(w, http.StatusInternalServerError, "backup_failed", fmt.Sprintf("inspect backup: %v", err))
					return
				}
				writeJSON(w, http.StatusCreated, protocol.BackupResponse{Destination: input.Destination, ByteSize: info.Size()})
			})
		}
		if cfg.ApplyGitIntent != nil {
			mux.HandleFunc("POST /v1/git-intents/{intent_id}/apply", func(w http.ResponseWriter, request *http.Request) {
				var input protocol.ApplyGitIntentRequest
				if !decodeJSON(w, request, &input) {
					return
				}
				intentID := request.PathValue("intent_id")
				appliedSHA, err := cfg.ApplyGitIntent(request.Context(), intentID, input.Message)
				if err != nil {
					writeError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, protocol.ApplyGitIntentResponse{IntentID: intentID, AppliedSHA: appliedSHA})
			})
		}
		if cfg.IngestProviderEvent != nil || cfg.IngestProviderHookEvent != nil {
			mux.HandleFunc("POST /v1/provider-events/{provider}", func(w http.ResponseWriter, request *http.Request) {
				var input protocol.ProviderEventRequest
				if !decodeProviderEventJSON(w, request, &input) {
					return
				}
				var err error
				if cfg.IngestProviderHookEvent != nil {
					err = cfg.IngestProviderHookEvent(request.Context(), request.PathValue("provider"), input.AgentSessionID, input.HookEventName, input.Event)
				} else {
					err = cfg.IngestProviderEvent(request.Context(), request.PathValue("provider"), input.AgentSessionID, input.Event)
				}
				if err != nil {
					writeError(w, err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
		}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set(protocol.VersionHeader, fmt.Sprintf("%d", protocol.Version))
		mux.ServeHTTP(w, request)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	if cfg.AfterListen != nil {
		if err := cfg.AfterListen(ctx); err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
				return fmt.Errorf("run daemon after-listen hook: %v; shut down daemon: %w", err, shutdownErr)
			}
			serveResult := <-serveErr
			if serveResult != nil && !errors.Is(serveResult, http.ErrServerClosed) {
				return fmt.Errorf("run daemon after-listen hook: %v; serve daemon: %w", err, serveResult)
			}
			return fmt.Errorf("run daemon after-listen hook: %w", err)
		}
	}
	drainResult := make(chan error, 1)
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	defer stopMonitor()
	if cfg.DrainReady != nil {
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				ready, err := cfg.DrainReady(monitorCtx)
				if err != nil {
					drainResult <- err
					return
				}
				if ready {
					drainResult <- nil
					return
				}
				select {
				case <-monitorCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down daemon: %w", err)
		}
		err := <-serveErr
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve daemon: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve daemon: %w", err)
		}
		return nil
	case initErr := <-initFailures:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down daemon after init failure: %w", err)
		}
		serveResult := <-serveErr
		if serveResult != nil && !errors.Is(serveResult, http.ErrServerClosed) {
			return fmt.Errorf("serve daemon after init failure: %w", serveResult)
		}
		return fmt.Errorf("initialization failed: %w", initErr)
	case drainErr := <-drainResult:
		if drainErr != nil {
			return fmt.Errorf("observe graceful drain: %w", drainErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down drained daemon: %w", err)
		}
		err := <-serveErr
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve drained daemon: %w", err)
		}
		return nil
	}
}

func isNotInitialized(ctx context.Context, service symphony.Symphony) bool {
	_, err := service.Inspect(ctx, symphony.Status{})
	var domainErr *symphony.DomainError
	return errors.As(err, &domainErr) && domainErr.Code == symphony.CodeNotInitialized
}

func decodeJSON(w http.ResponseWriter, request *http.Request, output any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("decode request: %v", err))
		return false
	}
	return true
}

func decodeProviderEventJSON(w http.ResponseWriter, request *http.Request, output any) bool {
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("decode request: %v", err))
		return false
	}
	return true
}

func commandMeta(mutation protocol.Mutation) symphony.CommandMeta {
	return symphony.CommandMeta{RequestID: mutation.RequestID, ExpectedRevision: mutation.ExpectedRevision}
}

func applyAndWrite(w http.ResponseWriter, request *http.Request, service symphony.Symphony, afterCommit func(context.Context), command symphony.Command) {
	receipt, err := service.Apply(request.Context(), command)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
	if afterCommit != nil {
		afterCommit(request.Context())
	}
}

func writeError(w http.ResponseWriter, err error) {
	var domainErr *symphony.DomainError
	if errors.As(err, &domainErr) {
		status := http.StatusConflict
		if domainErr.Code == symphony.CodeInvalidCommand {
			status = http.StatusBadRequest
		}
		writeAPIError(w, status, string(domainErr.Code), domainErr.Message)
		return
	}
	writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, protocol.ErrorBody{Error: protocol.APIError{Code: code, Message: message, Details: map[string]any{}}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func prepareSocket(socketPath string) error {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("inspect socket directory: %w", err)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("socket directory is not a directory: %s", dir)
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("socket directory must be accessible only to the current user: %s", dir)
	}

	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket path exists and is not a unix socket: %s", socketPath)
	}

	conn, dialErr := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon already listening on %s", socketPath)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}
