package registerbot

import "testing"

func TestBrowserProfilesReuseBoundedIsolatedSlots(t *testing.T) {
	settings := DefaultSettings()
	settings.Browser.PoolSize = 2
	settings.Browser.ProfileBaseDir = t.TempDir()
	pool := NewPool(func() Settings { return settings }, t.TempDir())
	if pool.profileDir(0) != pool.profileDir(2) || pool.profileDir(1) != pool.profileDir(3) {
		t.Fatal("retired browsers did not reuse bounded profile slots")
	}
	if pool.profileDir(0) == pool.profileDir(1) {
		t.Fatal("parallel browser slots share one profile")
	}
}

func TestDocumentReadyDoesNotWaitForLateSubresources(t *testing.T) {
	if documentReady("loading") || !documentReady("interactive") || !documentReady("complete") {
		t.Fatal("unexpected document readiness boundary")
	}
}
