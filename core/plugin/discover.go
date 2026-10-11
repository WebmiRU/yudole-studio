package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Descriptor is a plugin the kernel can run.
type Descriptor struct {
	// Name is the plugin id, taken from its directory.
	Name string
	// Path is the binary to execute.
	Path string
}

// Discover looks for chat plugins under root, which is expected to hold one
// directory per plugin with a binary of the same name inside:
//
//	root/goodgame/goodgame        (linux, macos)
//	root/goodgame/goodgame.exe    (windows)
//
// A directory without a binary is simply a plugin that has not been built yet.
// A missing root is not an error: plugins are optional.
func Discover(root string) ([]Descriptor, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugins dir %q: %w", root, err)
	}

	var found []Descriptor
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		name := entry.Name()
		path, ok, err := binaryPath(root, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		found = append(found, Descriptor{Name: name, Path: path})
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found, nil
}

// binaryPath finds the executable of a plugin, in the naming convention of the
// platform we run on: goodgame on unix, goodgame.exe on windows.
func binaryPath(root, name string) (string, bool, error) {
	for _, candidate := range binaryNames(name) {
		path := filepath.Join(root, name, candidate)

		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("plugin %q: %w", name, err)
		}
		if info.IsDir() {
			continue
		}
		// Unix checks the executable bit; windows has none, there the .exe
		// extension is what makes a file runnable.
		if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
			return "", false, fmt.Errorf("plugin %q: %s is not executable", name, path)
		}

		return path, true, nil
	}
	return "", false, nil
}

// binaryNames lists the file names a plugin binary may have, best first.
func binaryNames(name string) []string {
	if runtime.GOOS == "windows" {
		return []string{name + ".exe", name}
	}
	return []string{name}
}
