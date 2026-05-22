package store

import (
	"path/filepath"
	"testing"

	"gohome/shared/protocol"
)

func TestCanDeviceAccessFamily(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "go-home.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	publicID, err := store.CreateFamily("Public", protocol.FamilyVisibilityPublic)
	if err != nil {
		t.Fatalf("create public family: %v", err)
	}
	privateID, err := store.CreateFamily("Private", protocol.FamilyVisibilityPrivate)
	if err != nil {
		t.Fatalf("create private family: %v", err)
	}
	if err := store.UpsertDevice(protocol.DeviceAuthParams{
		DeviceID:   "client-a",
		DeviceType: protocol.DeviceTypeClient,
	}, "token", "127.0.0.1:1000"); err != nil {
		t.Fatalf("upsert client: %v", err)
	}

	assertAccess(t, store, "client-a", publicID, true)
	assertAccess(t, store, "client-a", privateID, false)
	if err := store.GrantFamilyDevice(privateID, "client-a"); err != nil {
		t.Fatalf("grant private family: %v", err)
	}
	assertAccess(t, store, "client-a", privateID, true)
}

func TestUpdateLANReportsChanges(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "go-home.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.UpsertDevice(protocol.DeviceAuthParams{
		DeviceID:   "home-a",
		DeviceType: protocol.DeviceTypeHomeServer,
	}, "token", "127.0.0.1:2000"); err != nil {
		t.Fatalf("upsert home server: %v", err)
	}

	changed, err := store.UpdateLAN("home-a", "192.168.3.0/24")
	if err != nil || !changed {
		t.Fatalf("first LAN update changed=%v err=%v", changed, err)
	}
	changed, err = store.UpdateLAN("home-a", "192.168.3.0/24")
	if err != nil || changed {
		t.Fatalf("same LAN update changed=%v err=%v", changed, err)
	}
}

func assertAccess(t *testing.T, store *Store, deviceID string, familyID int64, want bool) {
	t.Helper()
	got, err := store.CanDeviceAccessFamily(deviceID, familyID)
	if err != nil {
		t.Fatalf("access check: %v", err)
	}
	if got != want {
		t.Fatalf("access check got %v want %v", got, want)
	}
}
