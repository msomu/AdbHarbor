package harbor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteSubmitResultJSONLast(t *testing.T) {
	t.Setenv("ADB_HARBOR_DIR", t.TempDir())
	r := &Run{ID: "abc12def", Status: "succeeded", Serial: "PIXEL9"}
	var buf bytes.Buffer
	writeSubmitResult(&buf, r)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	last := lines[len(lines)-1]
	var got struct{ ID string `json:"id"` }
	if err := json.Unmarshal([]byte(last), &got); err != nil {
		t.Fatalf("last line %q: %v", last, err)
	}
	if got.ID != "abc12def" {
		t.Fatalf("id=%q", got.ID)
	}
	if strings.Contains(last, "logcat") {
		t.Fatalf("last line still looks like logcat: %q", last)
	}
}
