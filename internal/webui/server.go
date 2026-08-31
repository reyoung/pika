// Package webui serves the embedded read-only Workbench application and
// proxies a deliberately narrow GET-only API to a Workspace daemon.
package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed dist
var distribution embed.FS

const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

func NewHandler(socketPath, token string) (http.Handler, error) {
	if socketPath == "" || token == "" {
		return nil, errors.New("daemon socket and WebUI token are required")
	}
	assets, err := fs.Sub(distribution, "dist")
	if err != nil {
		return nil, err
	}
	target, _ := url.Parse("http://pika-go")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
		ResponseHeaderTimeout: 10 * time.Second,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, `{"error":{"code":"daemon_unavailable","message":%q}}`, err.Error())
	}
	fileServer := http.FileServer(http.FS(assets))
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, err
	}
	wantedToken := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		setSecurityHeaders(w)
		if strings.HasPrefix(request.URL.Path, "/api/") {
			if request.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "read-only WebUI", http.StatusMethodNotAllowed)
				return
			}
			if !allowedAPIPath(strings.TrimPrefix(request.URL.Path, "/api")) {
				http.NotFound(w, request)
				return
			}
			supplied := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
			suppliedDigest := sha256.Sum256([]byte(supplied))
			if supplied == "" || subtle.ConstantTimeCompare(wantedToken[:], suppliedDigest[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pika-go-webui"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"valid WebUI token required"}}`))
				return
			}
			request.Header.Del("Authorization")
			request.URL.Path = strings.TrimPrefix(request.URL.Path, "/api")
			proxy.ServeHTTP(w, request)
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clean := filepath.ToSlash(filepath.Clean(request.URL.Path))
		if clean != "." && clean != "/" {
			if _, err := fs.Stat(assets, strings.TrimPrefix(clean, "/")); err != nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				http.ServeContent(w, request, "index.html", time.Time{}, bytes.NewReader(index))
				return
			}
		}
		fileServer.ServeHTTP(w, request)
	}), nil
}

func allowedAPIPath(path string) bool {
	if path == "/v1/workbench/version" || path == "/v1/workbench/snapshot" {
		return true
	}
	return strings.HasPrefix(path, "/v1/workbench/nodes/") || strings.HasPrefix(path, "/v1/workbench/artifacts/")
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
}

func EnsureToken(runtimeRoot string, rotate bool) (string, string, error) {
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) {
		return "", "", errors.New("absolute runtime root is required")
	}
	directory := filepath.Join(runtimeRoot, "webui")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", "", err
	}
	path := filepath.Join(directory, "token")
	if !rotate {
		if token, err := readToken(path); err == nil {
			return token, path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
	}
	token, err := generateToken()
	if err != nil {
		return "", "", err
	}
	if rotate {
		temporary, err := os.CreateTemp(directory, ".token-")
		if err != nil {
			return "", "", err
		}
		temporaryPath := temporary.Name()
		defer os.Remove(temporaryPath)
		if err := temporary.Chmod(0o600); err != nil {
			_ = temporary.Close()
			return "", "", err
		}
		if _, err := temporary.WriteString(token + "\n"); err != nil {
			_ = temporary.Close()
			return "", "", err
		}
		if err := temporary.Sync(); err != nil {
			_ = temporary.Close()
			return "", "", err
		}
		if err := temporary.Close(); err != nil {
			return "", "", err
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			return "", "", err
		}
		return token, path, nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		token, err := readToken(path)
		return token, path, err
	}
	if err != nil {
		return "", "", err
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return "", "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", "", err
	}
	if err := file.Close(); err != nil {
		return "", "", err
	}
	return token, path, nil
}

func readToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", errors.New("WebUI token must be a regular 0600 file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", errors.New("WebUI token is empty")
	}
	return token, nil
}

func generateToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
