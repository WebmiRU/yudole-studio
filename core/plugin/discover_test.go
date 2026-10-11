package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()

	makePlugin(t, root, "goodgame")
	makePlugin(t, root, "twitch")
	makePluginDir(t, root, "not-built") // папка есть, бинарника нет

	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	descriptors, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	want := []string{"goodgame", "twitch"}
	if len(descriptors) != len(want) {
		t.Fatalf("found %d plugins (%v), want %d", len(descriptors), names(descriptors), len(want))
	}
	for i, name := range want {
		if descriptors[i].Name != name {
			t.Errorf("plugins[%d] = %q, want %q (sorted)", i, descriptors[i].Name, name)
		}
		wantPath := filepath.Join(root, name, binaryNames(name)[0])
		if descriptors[i].Path != wantPath {
			t.Errorf("%s path = %q, want %q", name, descriptors[i].Path, wantPath)
		}
	}
}

// Windows names plugin binaries goodgame.exe: when both are present, the one
// for the running platform wins.
func TestDiscoverPrefersPlatformBinary(t *testing.T) {
	root := t.TempDir()
	makePluginDir(t, root, "goodgame")
	for _, name := range []string{"goodgame", "goodgame.exe"} {
		if err := os.WriteFile(filepath.Join(root, "goodgame", name), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	descriptors, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("found %v, want one plugin", names(descriptors))
	}

	want := "goodgame"
	if runtime.GOOS == "windows" {
		want = "goodgame.exe"
	}
	if got := filepath.Base(descriptors[0].Path); got != want {
		t.Errorf("binary = %q, want %q on %s", got, want, runtime.GOOS)
	}
}

// A binary without the executable bit is a mistake worth reporting.
func TestDiscoverRejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits only")
	}

	root := t.TempDir()
	makePlugin(t, root, "twitch")
	makePluginDir(t, root, "goodgame")
	if err := os.WriteFile(filepath.Join(root, "goodgame", "goodgame"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Discover(root); err == nil {
		t.Fatal("expected an error for a non-executable plugin")
	}
}

// A repository without plugins is fine: nothing to start is not an error.
func TestDiscoverMissingDir(t *testing.T) {
	descriptors, err := Discover(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(descriptors) != 0 {
		t.Errorf("found %v, want none", descriptors)
	}
}

func TestBinaryNames(t *testing.T) {
	got := binaryNames("goodgame")
	if len(got) == 0 || got[0] != "goodgame" {
		t.Fatalf("binaryNames = %v", got)
	}

	if runtime.GOOS == "windows" {
		if !slices.Contains(got, "goodgame.exe") {
			t.Errorf("binaryNames = %v, want it to include goodgame.exe on windows", got)
		}
		return
	}
	if slices.Contains(got, "goodgame.exe") {
		t.Errorf("binaryNames = %v, want no .exe on unix", got)
	}
}

func names(descriptors []Descriptor) []string {
	out := make([]string, 0, len(descriptors))
	for _, d := range descriptors {
		out = append(out, d.Name)
	}
	return out
}

// makePluginDir creates root/name without a binary.
func makePluginDir(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}
}

// makePlugin creates root/name/<binary> with the executable bit set.
func makePlugin(t *testing.T, root, name string) {
	t.Helper()

	makePluginDir(t, root, name)
	path := filepath.Join(root, name, binaryNames(name)[0])
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake plugins are shell scripts")
	}
}
