package harbor

import (
	"bytes"
	"errors"
	"testing"
)

func TestExposeMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := runExpose("cloudflare", ioDiscard{}, ioDiscard{})
	var miss missingBinaryError
	if !errors.As(err, &miss) {
		t.Fatalf("got %v, want missingBinaryError", err)
	}
	if miss.bin != "cloudflared" || miss.brew != "cloudflared" {
		t.Fatalf("bin=%q brew=%q", miss.bin, miss.brew)
	}
	if got := err.Error(); !bytes.Contains([]byte(got), []byte("brew install cloudflared")) {
		t.Fatalf("error should print brew one-liner, got %q", got)
	}
}

func TestExposeUnknownVia(t *testing.T) {
	err := runExpose("not-a-vendor", ioDiscard{}, ioDiscard{})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("unknown --via")) {
		t.Fatalf("got %v", err)
	}
}

func TestFirstPublicURL(t *testing.T) {
	in := "INF | https://abc-def.trycloudflare.com |\nhttp://127.0.0.1:7437\n"
	if got := firstPublicURL(in); got != "https://abc-def.trycloudflare.com" {
		t.Fatalf("got %q", got)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
