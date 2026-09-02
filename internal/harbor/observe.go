package harbor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func serveObserve() error {
	token, err := loadOrCreateMCPToken()
	if err != nil {
		return err
	}
	start := LoadConfig().MCPPort
	if start <= 0 {
		start = 7437
	}
	ln, port, err := listenLocal(start)
	if err != nil {
		return err
	}
	if err := os.WriteFile(MCPPortPath(), []byte(fmt.Sprintf("%d\n", port)), 0o600); err != nil {
		return err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("observe listening on %s /mcp (bearer token in %s)", addr, MCPTokenPath())
	return http.Serve(ln, newObserveMux(token))
}

func listenLocal(start int) (net.Listener, int, error) {
	var last error
	for p := start; p < start+10; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, p, nil
		}
		last = err
	}
	return nil, 0, fmt.Errorf("no free port in %d-%d: %w", start, start+9, last)
}

func newObserveMux(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": Version})
	})
	mux.HandleFunc("GET /v1/runs", handleListRunsHTTP)
	mux.HandleFunc("GET /v1/runs/{id}", handleGetRunHTTP)
	mux.HandleFunc("POST /v1/runs", handlePostRunHTTP)
	h := newMCPHandler()
	mux.Handle("/mcp", h)
	mux.Handle("/mcp/", h)
	return requireBearer(token, mux)
}

func requireBearer(token string, next http.Handler) http.Handler {
	return auth.RequireBearerToken(func(_ context.Context, raw string, _ *http.Request) (*auth.TokenInfo, error) {
		if subtle.ConstantTimeCompare([]byte(raw), []byte(token)) != 1 {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Expiration: time.Now().Add(24 * time.Hour)}, nil
	}, nil)(next)
}

func loadOrCreateMCPToken() (string, error) {
	if err := EnsureDir(); err != nil {
		return "", err
	}
	if data, err := os.ReadFile(MCPTokenPath()); err == nil {
		if t := strings.TrimSpace(string(data)); t != "" {
			return t, nil
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b[:])
	if err := os.WriteFile(MCPTokenPath(), []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func newMCPHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "adbharbor", Version: Version}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "list_runs", Description: "List harbor proof runs"}, toolListRuns)
	mcp.AddTool(server, &mcp.Tool{Name: "get_run", Description: "Get one run by id"}, toolGetRun)
	mcp.AddTool(server, &mcp.Tool{Name: "wait_for_run", Description: "Wait until a run succeeds or fails"}, toolWaitForRun)
	mcp.AddTool(server, &mcp.Tool{Name: "get_proof", Description: "Return the run screenshot (PNG) and tailed logcat"}, toolGetProof)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}

type emptyIn struct{}

type idIn struct {
	ID string `json:"id" jsonschema:"run id"`
}

type waitIn struct {
	ID         string `json:"id" jsonschema:"run id"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"seconds to wait, default 120"`
}

func toolListRuns(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, []Run, error) {
	return nil, listRuns(), nil
}

func toolGetRun(_ context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, *Run, error) {
	r, err := loadRun(in.ID)
	return nil, r, err
}

func toolWaitForRun(_ context.Context, _ *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, *Run, error) {
	sec := in.TimeoutSec
	if sec <= 0 {
		sec = 120
	}
	deadline := time.Now().Add(time.Duration(sec) * time.Second)
	for {
		r, err := loadRun(in.ID)
		if err != nil {
			return nil, nil, err
		}
		if r.Status == "succeeded" || r.Status == "failed" {
			return nil, r, nil
		}
		if time.Now().After(deadline) {
			return nil, r, fmt.Errorf("timed out waiting for run %s (status %s)", r.ID, r.Status)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func toolGetProof(_ context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, *Run, error) {
	r, err := loadRun(in.ID)
	if err != nil {
		return nil, nil, err
	}
	png, err := os.ReadFile(r.ScreenshotPath())
	if err != nil {
		return nil, r, fmt.Errorf("no screenshot for run %s: %w", r.ID, err)
	}
	if !isPNG(png) {
		return nil, r, fmt.Errorf("run %s screenshot is not a PNG", r.ID)
	}
	logcat, _ := os.ReadFile(r.LogcatPath())
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{Data: png, MIMEType: "image/png"},
			&mcp.TextContent{Text: tailLines(string(logcat), 80)},
		},
	}, r, nil
}

func handleListRunsHTTP(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, listRuns())
}

func handleGetRunHTTP(w http.ResponseWriter, r *http.Request) {
	run, err := loadRun(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, run)
}

func handlePostRunHTTP(w http.ResponseWriter, r *http.Request) {
	run, err := runFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := saveRun(run); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go executeRun(run.ID)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, run)
}

func runFromRequest(r *http.Request) (*Run, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			return nil, err
		}
		pkg := r.FormValue("package")
		act := r.FormValue("activity")
		f, _, err := r.FormFile("apk")
		if err != nil {
			return nil, fmt.Errorf("apk file required")
		}
		defer f.Close()
		return newRunFromUpload(pkg, act, f)
	}
	var in struct {
		APK      string `json:"apk"`
		Package  string `json:"package"`
		Activity string `json:"activity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, err
	}
	return newRunFromPath(in.APK, in.Package, in.Activity)
}

func observeAddr() (string, error) {
	if data, err := os.ReadFile(MCPPortPath()); err == nil {
		if p := strings.TrimSpace(string(data)); p != "" {
			addr := "127.0.0.1:" + p
			if dialOK(addr) {
				return addr, nil
			}
		}
	}
	port := LoadConfig().MCPPort
	if port <= 0 {
		port = 7437
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if !dialOK(addr) {
		return "", fmt.Errorf("observe listener not up on %s — start the daemon", addr)
	}
	return addr, nil
}

func dialOK(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
