package engine

import (
	"os"
	"path/filepath"
	"regexp"
)

// consoleSocketName matches ConsoleSocketPath. Eight hash bytes print as
// sixteen hex digits. Anything else in the directory is left alone.
var consoleSocketName = regexp.MustCompile(`^darwin-console-[0-9a-f]{16}\.sock$`)

// SweepConsoleSockets removes break-glass sockets left in dir by a crashed
// process. dir is os.TempDir() at process start, before any pod listens.
// A directory with a matching name is not removed.
func SweepConsoleSockets(dir string) error {
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var first error
	for _, ent := range ents {
		if ent.IsDir() || !consoleSocketName.MatchString(ent.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, ent.Name())); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}
