package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/reyoung/pika-go/internal/toolapp"
)

const ProtocolVersion = "2025-06-18"

type Handler struct {
	Application   toolapp.Application
	AfterMutation func()
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.JSONRPC != "2.0" || input.Method == "" {
		writeResponse(w, http.StatusBadRequest, response{JSONRPC: "2.0", ID: input.ID, Error: &rpcError{Code: -32600, Message: "invalid JSON-RPC request"}})
		return
	}
	if len(input.ID) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	result, mutated, err := h.dispatch(r, token, input)
	if err != nil {
		writeResponse(w, http.StatusOK, response{JSONRPC: "2.0", ID: input.ID, Error: &rpcError{Code: -32001, Message: err.Error()}})
		return
	}
	writeResponse(w, http.StatusOK, response{JSONRPC: "2.0", ID: input.ID, Result: result})
	if mutated && h.AfterMutation != nil {
		h.AfterMutation()
	}
}

func (h Handler) dispatch(r *http.Request, token string, input request) (any, bool, error) {
	switch input.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "pika-go", "version": "dev"},
		}, false, nil
	case "ping":
		return map[string]any{}, false, nil
	case "tools/list":
		tools, err := h.Application.Catalog(r.Context(), token)
		if err != nil {
			return nil, false, err
		}
		return map[string]any{"tools": tools}, false, nil
	case "tools/call":
		var params toolapp.Call
		if err := json.Unmarshal(input.Params, &params); err != nil || params.Name == "" {
			return nil, false, errors.New("invalid tools/call parameters")
		}
		invocation, err := h.Application.Invoke(r.Context(), token, params)
		if err != nil {
			return nil, false, err
		}
		encoded, err := json.Marshal(invocation.Value)
		if err != nil {
			return nil, false, fmt.Errorf("encode tool result: %w", err)
		}
		return map[string]any{"content": []map[string]string{{"type": "text", "text": string(encoded)}}, "isError": false}, invocation.Mutated, nil
	default:
		return nil, false, fmt.Errorf("method %q is not supported", input.Method)
	}
}

func writeResponse(w http.ResponseWriter, status int, output response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(output)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
