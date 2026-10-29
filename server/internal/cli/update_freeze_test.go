package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckUpdateFreeze(t *testing.T) {
	file := filepath.Join(t.TempDir(), "UPDATE_FREEZE")
	t.Setenv(UpdateFreezeFileEnv, file)

	if err := CheckUpdateFreeze(); err != nil {
		t.Fatalf("no freeze file: want nil, got %v", err)
	}

	if err := os.WriteFile(file, []byte("CHE-1234 owner packet pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckUpdateFreeze()
	if err == nil {
		t.Fatal("freeze file present: want error, got nil")
	}
	if !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "CHE-1234") {
		t.Fatalf("error should name the file and its reason, got %v", err)
	}
}

func TestCheckUpdateFreeze_NoHomeFailsClosed(t *testing.T) {
	t.Setenv(UpdateFreezeFileEnv, "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("home", "")
	if err := CheckUpdateFreeze(); err == nil {
		t.Fatal("no home and no override: want error, got nil")
	}
}
