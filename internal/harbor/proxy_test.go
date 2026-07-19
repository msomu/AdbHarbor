package harbor

import "testing"

func TestParseTransport(t *testing.T) {
	cases := []struct {
		req    string
		serial string
		tport  bool
		ok     bool
	}{
		{"host:transport:ABC123", "ABC123", false, true},
		{"host:tport:serial:ABC123", "ABC123", true, true},
		{"host:transport-any", "", false, true},
		{"host:tport:any", "", true, true},
		{"host:transport-usb", "", false, true},
		{"host:version", "", false, false},
		{"host:devices-l", "", false, false},
		{"host-serial:ABC:features", "", false, false},
		{"shell:ls", "", false, false},
	}
	for _, c := range cases {
		tr, ok := parseTransport(c.req)
		if ok != c.ok || tr.serial != c.serial || tr.tport != c.tport {
			t.Errorf("parseTransport(%q) = {%q %v} ok=%v, want {%q %v} ok=%v",
				c.req, tr.serial, tr.tport, ok, c.serial, c.tport, c.ok)
		}
	}
}

func TestIsExemptService(t *testing.T) {
	prefixes := DefaultConfig().ExemptShell
	cases := map[string]bool{
		"shell:getprop ro.product.model":            true,
		"shell,v2,TERM=xterm-256color:getprop ro.x": true,
		"exec:getprop ro.build.version.sdk":         true,
		"shell:dumpsys battery":                     true,
		"shell:pm list packages":                    true,
		"shell:settings get global adb_enabled":     true,
		"shell:am start -n com.foo/.Main":           false,
		"shell:pm install /data/local/tmp/app.apk":  false,
		"shell:pm uninstall com.foo":                false,
		"shell:input tap 100 200":                   false,
		"shell:":                                    false, // interactive shell
		"shell,v2,pty:":                             false,
		"sync:":                                     false,
		"exec:cmd package install-create":           false,
		"framebuffer:":                              false,
		"root:":                                     false,
		"shell:monkey -p com.foo 1":                 false,
	}
	for svc, want := range cases {
		if got := isExemptService(svc, prefixes); got != want {
			t.Errorf("isExemptService(%q) = %v, want %v", svc, got, want)
		}
	}
}

func TestEnvWithServerPort(t *testing.T) {
	env := []string{"PATH=/bin", "ANDROID_ADB_SERVER_PORT=5037", "HOME=/x"}
	got := envWithServerPort(env, 5038)
	found := false
	for _, e := range got {
		if e == "ANDROID_ADB_SERVER_PORT=5038" {
			found = true
		}
		if e == "ANDROID_ADB_SERVER_PORT=5037" {
			t.Error("old port entry should have been removed")
		}
	}
	if !found {
		t.Error("new port entry missing")
	}
}
