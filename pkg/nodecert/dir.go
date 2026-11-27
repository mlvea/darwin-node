package nodecert

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// PrepareDir creates path if it is missing and requires it to be a
// directory this process can keep private.
//
// The directory mode is 0700. A directory that is already group or world
// writable is refused and left unchanged: tightening it would hide the
// fact that someone else could have written into it. The path's final
// component is opened with O_NOFOLLOW, so a symlink (whose target is
// outside the directory inode) is refused. Symlinks inside the directory
// must point at a path that stays inside it. kubelet-*-current.pem is a
// symlink to a sibling PEM; a link that escapes the directory is not.
func PrepareDir(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("cert dir is required")
	}
	path = filepath.Clean(path)
	fi, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("cert dir: %w", err)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create cert dir: %w", err)
		}
	} else {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cert dir %s is a symlink", path)
		}
		if !fi.IsDir() {
			return fmt.Errorf("cert dir %s is not a directory", path)
		}
	}
	return openCertDir(path)
}

func openCertDir(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return fmt.Errorf("cert dir %s is a symlink", path)
		}
		return fmt.Errorf("open cert dir: %w", err)
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat cert dir: %w", err)
	}
	// 0022 is the group and world write bits. Refuse before any chmod so
	// a writable directory is still writable when the error is returned.
	if st.Mode&0022 != 0 {
		return fmt.Errorf("cert dir %s is group or world writable", path)
	}
	if err := linksInside(path); err != nil {
		return err
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return fmt.Errorf("chmod cert dir: %w", err)
	}
	return tightenFiles(path)
}

// linksInside refuses a symlink whose target leaves dir.
func linksInside(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read cert dir: %w", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat %s: %w", e.Name(), err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if err := linkInside(dir, filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func linkInside(dir, linkPath string) error {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return fmt.Errorf("readlink %s: %w", linkPath, err)
	}
	var full string
	if filepath.IsAbs(target) {
		full = filepath.Clean(target)
	} else {
		full = filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("cert dir: %w", err)
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return fmt.Errorf("symlink %s: %w", linkPath, err)
	}
	rel, err := filepath.Rel(dirAbs, fullAbs)
	if err != nil {
		return fmt.Errorf("symlink %s: %w", linkPath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("symlink %s points outside the cert dir", linkPath)
	}
	return nil
}

// tightenFiles forces regular files to 0600. Symlinks are not followed:
// O_NOFOLLOW fails with ELOOP and the target was already checked.
func tightenFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read cert dir: %w", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				continue
			}
			return fmt.Errorf("open %s: %w", e.Name(), err)
		}
		cerr := unix.Fchmod(fd, 0o600)
		unix.Close(fd)
		if cerr != nil {
			return fmt.Errorf("chmod %s: %w", e.Name(), cerr)
		}
	}
	return nil
}
