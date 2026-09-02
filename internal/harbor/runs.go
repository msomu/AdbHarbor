package harbor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

var pngMagic = []byte{0x89, 0x50, 0x4e, 0x47}

type Run struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Package   string    `json:"package"`
	Activity  string    `json:"activity"`
	APK       string    `json:"apk,omitempty"`
	Serial    string    `json:"serial,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r *Run) dir() string              { return filepath.Join(RunsDir(), r.ID) }
func (r *Run) metaPath() string         { return filepath.Join(r.dir(), "meta.json") }
func (r *Run) ScreenshotPath() string   { return filepath.Join(r.dir(), "screenshot.png") }
func (r *Run) LogcatPath() string       { return filepath.Join(r.dir(), "logcat.txt") }

func newRunID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func validRunID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func newRunFromPath(apk, pkg, activity string) (*Run, error) {
	if pkg == "" || activity == "" {
		return nil, fmt.Errorf("package and activity are required")
	}
	if apk == "" {
		return nil, fmt.Errorf("apk path is required")
	}
	abs, err := filepath.Abs(apk)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("apk: %w", err)
	}
	now := time.Now()
	return &Run{
		ID: newRunID(), Status: "queued", Package: pkg, Activity: activity,
		APK: abs, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func newRunFromUpload(pkg, activity string, src io.Reader) (*Run, error) {
	if pkg == "" || activity == "" {
		return nil, fmt.Errorf("package and activity are required")
	}
	now := time.Now()
	r := &Run{
		ID: newRunID(), Status: "queued", Package: pkg, Activity: activity,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := os.MkdirAll(r.dir(), 0o700); err != nil {
		return nil, err
	}
	dst := filepath.Join(r.dir(), "app.apk")
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	r.APK = dst
	return r, nil
}

func saveRun(r *Run) error {
	if err := os.MkdirAll(r.dir(), 0o700); err != nil {
		return err
	}
	r.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.metaPath(), append(data, '\n'), 0o600)
}

func loadRun(id string) (*Run, error) {
	if !validRunID(id) {
		return nil, fmt.Errorf("run %s not found", id)
	}
	data, err := os.ReadFile(filepath.Join(RunsDir(), id, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("run %s not found", id)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func listRuns() []Run {
	ents, err := os.ReadDir(RunsDir())
	if err != nil {
		return nil
	}
	var out []Run
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		r, err := loadRun(e.Name())
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func failRun(r *Run, err error) {
	r.Status = "failed"
	r.Error = err.Error()
	_ = saveRun(r)
}

func executeRun(id string) {
	r, err := loadRun(id)
	if err != nil {
		return
	}
	r.Status = "running"
	if err := saveRun(r); err != nil {
		return
	}
	cfg := LoadConfig()
	session := "run-" + r.ID
	resp, err := AcquireAny(AcquireAnyReq{
		Session: session,
		Holder:  "submit " + r.Package,
		PID:     os.Getpid(),
		TTLSec:  600,
	})
	if err != nil {
		failRun(r, err)
		return
	}
	if !resp.Granted {
		failRun(r, fmt.Errorf("%s", resp.Message))
		return
	}
	r.Serial = resp.Serial
	_ = saveRun(r)
	defer Release(ReleaseReq{LeaseID: resp.LeaseID, Session: session})

	if err := adbRun(cfg, r.Serial, 120*time.Second, "install", "-r", r.APK); err != nil {
		failRun(r, fmt.Errorf("install: %w", err))
		return
	}
	comp := launchComponent(r.Package, r.Activity)
	if err := adbRun(cfg, r.Serial, 30*time.Second, "shell", "am", "start", "-W", "-n", comp); err != nil {
		failRun(r, fmt.Errorf("launch: %w", err))
		return
	}
	png, err := adbOutput(cfg, r.Serial, 20*time.Second, "exec-out", "screencap", "-p")
	if err != nil {
		failRun(r, fmt.Errorf("screencap: %w", err))
		return
	}
	if !isPNG(png) {
		failRun(r, fmt.Errorf("screencap did not return a PNG (%d bytes)", len(png)))
		return
	}
	if err := os.WriteFile(r.ScreenshotPath(), png, 0o600); err != nil {
		failRun(r, err)
		return
	}
	if logcat, err := adbOutput(cfg, r.Serial, 15*time.Second, "logcat", "-d", "-t", "200"); err == nil {
		_ = os.WriteFile(r.LogcatPath(), logcat, 0o600)
	}
	r.Status = "succeeded"
	r.Error = ""
	_ = saveRun(r)
}

func launchComponent(pkg, activity string) string {
	if strings.Contains(activity, "/") {
		return activity
	}
	return pkg + "/" + activity
}

func isPNG(b []byte) bool { return bytes.HasPrefix(b, pngMagic) }

func adbCmd(ctx context.Context, cfg *Config, serial string, args ...string) *exec.Cmd {
	all := append([]string{"-s", serial}, args...)
	cmd := exec.CommandContext(ctx, cfg.RealADB, all...)
	if p := cfg.ClientServerPort(); p > 0 {
		cmd.Env = envWithServerPort(os.Environ(), p)
	}
	return cmd
}

func adbRun(cfg *Config, serial string, timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := adbCmd(ctx, cfg, serial, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func adbOutput(cfg *Config, serial string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return adbCmd(ctx, cfg, serial, args...).Output()
}

func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func CmdSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	apk := fs.String("apk", "", "path to APK")
	pkg := fs.String("package", "", "application id")
	act := fs.String("activity", "", "launch activity (pkg/.Main or pkg/cls)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apk == "" || *pkg == "" || *act == "" {
		return fmt.Errorf("submit: --apk, --package, and --activity are required")
	}
	if err := EnsureDaemon(); err != nil {
		return err
	}
	run, err := newRunFromPath(*apk, *pkg, *act)
	if err != nil {
		return err
	}
	if err := saveRun(run); err != nil {
		return err
	}
	executeRun(run.ID)
	done, err := loadRun(run.ID)
	if err != nil {
		return err
	}
	writeSubmitResult(os.Stdout, done)
	if done.Status != "succeeded" {
		return fmt.Errorf("%s", done.Error)
	}
	return nil
}

// writeSubmitResult prints human lines then a JSON object last so last-line
// parsers take {"id":"..."} instead of a logcat path.
func writeSubmitResult(w io.Writer, r *Run) {
	fmt.Fprintf(w, "run %s %s\n", r.ID, r.Status)
	if r.Serial != "" {
		fmt.Fprintf(w, "serial %s\n", r.Serial)
	}
	if r.Status == "succeeded" {
		fmt.Fprintf(w, "screenshot %s\n", r.ScreenshotPath())
		fmt.Fprintf(w, "logcat %s\n", r.LogcatPath())
	}
	fmt.Fprintf(w, "{\"id\":%q}\n", r.ID)
}
