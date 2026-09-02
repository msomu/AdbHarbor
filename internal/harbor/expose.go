package harbor

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type missingBinaryError struct {
	bin, brew string
}

func (e missingBinaryError) Error() string {
	return fmt.Sprintf("%s not on PATH\ninstall with: brew install %s", e.bin, e.brew)
}

type exposeBackend struct {
	bin, brew string
	args      func(addr string) []string
}

var exposeBackends = map[string]exposeBackend{
	"cloudflare": {
		bin: "cloudflared", brew: "cloudflared",
		args: func(addr string) []string {
			return []string{"tunnel", "--url", "http://" + addr, "--no-autoupdate"}
		},
	},
	"ngrok": {
		bin: "ngrok", brew: "ngrok",
		args: func(addr string) []string {
			_, port, _ := splitHostPort(addr)
			return []string{"http", port, "--log", "stdout"}
		},
	},
	"tailscale": {
		bin: "tailscale", brew: "tailscale",
		args: func(addr string) []string {
			_, port, _ := splitHostPort(addr)
			return []string{"funnel", port}
		},
	},
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", fmt.Errorf("no port")
	}
	return addr[:i], addr[i+1:], nil
}

var publicHTTPS = regexp.MustCompile(`https://[a-zA-Z0-9][-a-zA-Z0-9.]*[a-zA-Z0-9]`)

func firstPublicURL(s string) string {
	for _, m := range publicHTTPS.FindAllString(s, -1) {
		if strings.Contains(m, "127.0.0.1") || strings.Contains(m, "localhost") {
			continue
		}
		return strings.TrimRight(m, ".,);")
	}
	return ""
}

func CmdExpose(args []string) error {
	fs := flag.NewFlagSet("expose", flag.ContinueOnError)
	via := fs.String("via", "cloudflare", "tunnel backend: cloudflare, ngrok, tailscale")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runExpose(*via, os.Stdout, os.Stderr)
}

func runExpose(via string, stdout, stderr io.Writer) error {
	be, ok := exposeBackends[via]
	if !ok {
		return fmt.Errorf("expose: unknown --via %q (cloudflare, ngrok, tailscale)", via)
	}
	bin, err := exec.LookPath(be.bin)
	if err != nil {
		return missingBinaryError{bin: be.bin, brew: be.brew}
	}
	if err := EnsureDaemon(); err != nil {
		return err
	}
	addr, err := observeAddr()
	if err != nil {
		return err
	}
	token, err := loadOrCreateMCPToken()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, be.args(addr)...)
	cmd.Stdin = os.Stdin
	pr, pw := io.Pipe()
	cmd.Stdout = io.MultiWriter(stderr, pw)
	cmd.Stderr = io.MultiWriter(stderr, pw)
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	urlCh := make(chan string, 1)
	go func() {
		defer pw.Close()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var buf strings.Builder
		for sc.Scan() {
			buf.WriteString(sc.Text())
			buf.WriteByte('\n')
			if u := firstPublicURL(buf.String()); u != "" {
				select {
				case urlCh <- u:
				default:
				}
			}
		}
	}()
	var public string
	select {
	case public = <-urlCh:
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
		return fmt.Errorf("expose: %s started but no public HTTPS URL in 25s", be.bin)
	}
	mcpURL := strings.TrimRight(public, "/") + "/mcp"
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "public  %s\n", mcpURL)
	fmt.Fprintf(stdout, "token   %s\n", token)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Cursor Dashboard → MCP → HTTP:")
	fmt.Fprintf(stdout, "  url:     %s\n", mcpURL)
	fmt.Fprintf(stdout, "  header:  Authorization: Bearer %s\n", token)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, `{`)
	fmt.Fprintln(stdout, `  "adbharbor": {`)
	fmt.Fprintf(stdout, "    \"url\": %q,\n", mcpURL)
	fmt.Fprintln(stdout, `    "headers": {`)
	fmt.Fprintf(stdout, "      \"Authorization\": \"Bearer %s\"\n", token)
	fmt.Fprintln(stdout, `    }`)
	fmt.Fprintln(stdout, `  }`)
	fmt.Fprintln(stdout, `}`)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Ctrl-C stops the tunnel. Local Cursor still uses the CLI — no MCP.")
	return cmd.Wait()
}
