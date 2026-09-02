package harbor

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestObserveAuthReject(t *testing.T) {
	t.Setenv("ADB_HARBOR_DIR", t.TempDir())
	h := newObserveMux("secret-token")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d want 401", rr.Code)
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d want 401", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("good token: got %d want 200 body=%s", rr.Code, rr.Body.String())
	}
}

func TestSelectAcquireAnyDeniesSamsung(t *testing.T) {
	devs := []Device{
		{Serial: "RZGL41JKGFT", State: "device", USB: true},
		{Serial: "PIXEL9", State: "device", USB: true},
	}
	serial, busy, deniedN := selectAcquireAny(devs, nil, nil, []string{"RZGL41JKGFT"}, false, false)
	if serial != "PIXEL9" || deniedN != 1 || len(busy) != 0 {
		t.Fatalf("serial=%s busy=%v denied=%d", serial, busy, deniedN)
	}
}

func TestSelectAcquireAnyAllDenied(t *testing.T) {
	devs := []Device{{Serial: "RZGL41JKGFT", State: "device", USB: true}}
	serial, busy, deniedN := selectAcquireAny(devs, nil, nil, []string{"RZGL41JKGFT"}, false, false)
	if serial != "" || deniedN != 1 || len(busy) != 0 {
		t.Fatalf("serial=%s busy=%v denied=%d", serial, busy, deniedN)
	}
}

func TestDeniedSerial(t *testing.T) {
	if !deniedSerial("RZGL41JKGFT", []string{"RZGL41JKGFT"}) {
		t.Fatal("expected deny")
	}
	if deniedSerial("PIXEL9", []string{"RZGL41JKGFT"}) {
		t.Fatal("PIXEL9 should be allowed")
	}
}
