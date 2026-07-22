package harbor

import (
	"reflect"
	"testing"
)

func TestNewPackagesToRemove(t *testing.T) {
	protected := DefaultConfig().ProtectedPackages
	baseline := []string{"com.example.existing", "com.android.chrome", "org.other.app"}
	current := []string{
		"com.example.existing",    // pre-existing: keep
		"com.android.chrome",      // pre-existing + protected: keep
		"com.example.newapp",      // session-installed: remove
		"com.example.newapp.test", // session-installed test pkg: remove
		"com.google.newthing",     // appeared but protected prefix: keep
		"android.autoinstalled",   // protected prefix "android": keep
		"org.other.app",           // pre-existing: keep
	}
	got := newPackagesToRemove(current, baseline, protected)
	want := []string{"com.example.newapp", "com.example.newapp.test"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("newPackagesToRemove = %v, want %v", got, want)
	}
}

func TestNewPackagesToRemoveNoBaseline(t *testing.T) {
	// Empty baseline means everything unprotected would be flagged — the
	// broker guards against this by skipping cleanup when no baseline was
	// captured, but the pure function should still behave predictably.
	got := newPackagesToRemove([]string{"com.a", "com.android.x"}, nil, DefaultConfig().ProtectedPackages)
	if !reflect.DeepEqual(got, []string{"com.a"}) {
		t.Errorf("got %v", got)
	}
}

func TestIsProtectedPackage(t *testing.T) {
	protected := DefaultConfig().ProtectedPackages
	for pkg, want := range map[string]bool{
		"com.android.settings":  true,
		"com.google.android.gm": true,
		"android":               true,
		"com.samsung.knox":      true,
		"com.example.app":       false,
		"androidx.test.runner":  true, // "android" prefix — conservative
	} {
		if got := isProtectedPackage(pkg, protected); got != want {
			t.Errorf("isProtectedPackage(%q) = %v, want %v", pkg, got, want)
		}
	}
}
