package volume

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// volFile is one regular file in a ConfigMap, Secret, projected, or
// downwardAPI volume. The bytes are the node's, never whatever a symlink
// in the volume directory points at.
type volFile struct {
	data []byte
	mode os.FileMode
}

// publishVolume projects files into dir and swaps them into place.
//
// The directory inode stays put so a virtio-fs share of dir keeps working.
// Payload bytes are written with O_NOFOLLOW into a new timestamped directory,
// then a relative "..data" symlink is renamed over the previous one. That
// rename is the atomic publish, the same shape as kubelet's AtomicWriter.
// User-visible names are relative symlinks to "..data/<name>". rename
// replaces a symlink or hard link; it does not open it. A volume directory
// that is itself a symlink is replaced from its parent and is not followed.
//
// Names starting with ".." are reserved for the timestamp directories.
func publishVolume(dir string, files map[string]volFile) error {
	files, err := cleanPayload(files)
	if err != nil {
		return err
	}
	parent := filepath.Dir(dir)
	base := filepath.Base(dir)
	parentFd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return volErr(err)
	}
	defer unix.Close(parentFd)

	var st unix.Stat_t
	err = unix.Fstatat(parentFd, base, &st, unix.AT_SYMLINK_NOFOLLOW)
	kind := statType(uint64(st.Mode))
	switch {
	case err == nil && kind == unix.S_IFLNK:
		return replaceSymlinkedVolume(parentFd, base, files)
	case err == nil && kind != unix.S_IFDIR:
		return fmt.Errorf("volume path is not a directory")
	case err != nil && !os.IsNotExist(err):
		return volErr(err)
	case os.IsNotExist(err):
		if err := unix.Mkdirat(parentFd, base, uint32(dirPerm)); err != nil {
			return volErr(err)
		}
	}
	dirFd, err := unix.Openat(parentFd, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return volErr(err)
	}
	defer unix.Close(dirFd)
	if err := unix.Fchmod(dirFd, uint32(dirPerm)); err != nil {
		return volErr(err)
	}
	return publishInto(dirFd, files)
}

func replaceSymlinkedVolume(parentFd int, base string, files map[string]volFile) (err error) {
	tmp := ".vol-" + randSuffix()
	if err = unix.Mkdirat(parentFd, tmp, uint32(dirPerm)); err != nil {
		return volErr(err)
	}
	defer func() {
		if err != nil {
			_ = removeTreeNoFollow(parentFd, tmp)
		}
	}()
	fd, oerr := unix.Openat(parentFd, tmp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if oerr != nil {
		return volErr(oerr)
	}
	defer unix.Close(fd)
	if err = unix.Fchmod(fd, uint32(dirPerm)); err != nil {
		return volErr(err)
	}
	if err = publishInto(fd, files); err != nil {
		return err
	}
	// Linux rename of a directory onto a symlink fails (ENOTDIR) and leaves
	// the symlink in place. Unlink removes that symlink, not its target.
	if err = unix.Renameat(parentFd, tmp, parentFd, base); err != nil {
		var st unix.Stat_t
		if serr := unix.Fstatat(parentFd, base, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil && statType(uint64(st.Mode)) == unix.S_IFLNK {
			if uerr := unix.Unlinkat(parentFd, base, 0); uerr != nil {
				return volErr(uerr)
			}
			err = unix.Renameat(parentFd, tmp, parentFd, base)
		}
	}
	if err != nil {
		return volErr(err)
	}
	return nil
}

func publishInto(dirFd int, files map[string]volFile) (err error) {
	tsName, err := makeTimestampDir(dirFd)
	if err != nil {
		return err
	}
	swapped := false
	defer func() {
		if err != nil && !swapped {
			_ = removeTreeNoFollow(dirFd, tsName)
			_ = removeTreeNoFollow(dirFd, "..data_tmp")
		}
	}()
	for rel, file := range files {
		if werr := writePayloadFile(dirFd, filepath.Join(tsName, rel), file.data, file.mode); werr != nil {
			return volErr(werr)
		}
	}
	if err = fsyncDirAt(dirFd, tsName); err != nil {
		return err
	}
	_ = removeTreeNoFollow(dirFd, "..data_tmp")
	if err = unix.Symlinkat(tsName, dirFd, "..data_tmp"); err != nil {
		return volErr(err)
	}
	if err = unix.Renameat(dirFd, "..data_tmp", dirFd, "..data"); err != nil {
		return volErr(err)
	}
	swapped = true
	_ = unix.Fsync(dirFd)

	tops := topNames(files)
	for name := range tops {
		if err = ensureVisibleLink(dirFd, name); err != nil {
			return volErr(err)
		}
	}
	if err = removeStaleVisible(dirFd, tops); err != nil {
		return err
	}
	return sweepOldData(dirFd, tsName)
}

func makeTimestampDir(dirFd int) (string, error) {
	for i := 0; i < 8; i++ {
		name := time.Now().UTC().Format("..2006_01_02_15_04_05.") + randSuffix()
		if !validTSName(name) {
			continue
		}
		err := unix.Mkdirat(dirFd, name, uint32(dirPerm))
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", volErr(err)
		}
		fd, oerr := unix.Openat(dirFd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if oerr != nil {
			_ = removeTreeNoFollow(dirFd, name)
			return "", volErr(oerr)
		}
		cerr := unix.Fchmod(fd, uint32(dirPerm))
		unix.Close(fd)
		if cerr != nil {
			_ = removeTreeNoFollow(dirFd, name)
			return "", volErr(cerr)
		}
		return name, nil
	}
	return "", fmt.Errorf("volume data directory")
}

func writePayloadFile(rootFd int, rel string, data []byte, mode os.FileMode) error {
	parts := strings.Split(rel, string(filepath.Separator))
	fd, err := unix.Dup(rootFd)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range parts[:len(parts)-1] {
		if err := unix.Mkdirat(fd, part, uint32(dirPerm)); err != nil && !os.IsExist(err) {
			return err
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		if err := unix.Fchmod(next, uint32(dirPerm)); err != nil {
			unix.Close(next)
			return err
		}
		_ = unix.Close(fd)
		fd = next
	}
	part := parts[len(parts)-1]
	wfd, err := unix.Openat(fd, part, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(wfd), "volume")
	werr := writeFull(f, data)
	if werr == nil {
		werr = f.Chmod(mode.Perm())
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func ensureVisibleLink(dirFd int, name string) error {
	want := "..data/" + name
	var buf [4096]byte
	n, err := unix.Readlinkat(dirFd, name, buf[:])
	if err == nil && n < len(buf) && string(buf[:n]) == want {
		return nil
	}
	tmp := "..tmplink-" + randSuffix()
	if err := unix.Symlinkat(want, dirFd, tmp); err != nil {
		return err
	}
	if err := unix.Renameat(dirFd, tmp, dirFd, name); err == nil {
		return nil
	} else if !isDirRenameErr(err) {
		_ = unix.Unlinkat(dirFd, tmp, 0)
		return err
	}
	// A real directory occupies the visible name (the layout from before
	// atomic publish, or a directory planted in the volume). Move it aside
	// and remove it as a tree. rename of the symlink did not follow it.
	trash := "..trash-" + randSuffix()
	if rerr := unix.Renameat(dirFd, name, dirFd, trash); rerr != nil {
		_ = unix.Unlinkat(dirFd, tmp, 0)
		return err
	}
	if rerr := unix.Renameat(dirFd, tmp, dirFd, name); rerr != nil {
		return rerr
	}
	return removeTreeNoFollow(dirFd, trash)
}

func removeStaleVisible(dirFd int, keep map[string]struct{}) error {
	names, err := readDirFd(dirFd)
	if err != nil {
		return volErr(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "..") || strings.HasPrefix(name, ".vol-") {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		if err := removeTreeNoFollow(dirFd, name); err != nil {
			return volErr(err)
		}
	}
	return nil
}

func sweepOldData(dirFd int, current string) error {
	names, err := readDirFd(dirFd)
	if err != nil {
		return volErr(err)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "..") {
			continue
		}
		if name == "..data" || name == current {
			continue
		}
		if err := removeTreeNoFollow(dirFd, name); err != nil {
			return volErr(err)
		}
	}
	return nil
}

func removeTreeNoFollow(parentFd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(parentFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if statType(uint64(st.Mode)) != unix.S_IFDIR {
		return unix.Unlinkat(parentFd, name, 0)
	}
	fd, err := unix.Openat(parentFd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		// A symlink raced in. Do not walk it.
		if errors.Is(err, unix.ELOOP) {
			return unix.Unlinkat(parentFd, name, 0)
		}
		return err
	}
	defer unix.Close(fd)
	names, err := readDirFd(fd)
	if err != nil {
		return err
	}
	for _, child := range names {
		if err := removeTreeNoFollow(fd, child); err != nil {
			return err
		}
	}
	return unix.Unlinkat(parentFd, name, unix.AT_REMOVEDIR)
}

func readDirFd(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "volume")
	defer f.Close()
	ents, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

func fsyncDirAt(dirFd int, name string) error {
	fd, err := unix.Openat(dirFd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return volErr(err)
	}
	defer unix.Close(fd)
	return volErr(unix.Fsync(fd))
}

func readPublished(dir string) (map[string]volFile, error) {
	out := map[string]volFile{}
	fi, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, volErr(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// Replacing the directory. Do not read the symlink target.
		return out, nil
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("volume path is not a directory")
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, volErr(err)
	}
	defer unix.Close(fd)
	readRoot := dir
	if dataName, lerr := readDataLink(fd); lerr == nil {
		readRoot = filepath.Join(dir, dataName)
	} else if !os.IsNotExist(lerr) {
		// The data link is not a relative timestamp name. Do not follow it.
		return out, nil
	}
	return readRegularFiles(readRoot)
}

func readRegularFiles(root string) (map[string]volFile, error) {
	out := map[string]volFile{}
	fi, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, volErr(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, fmt.Errorf("volume data path is not a directory")
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return volErr(err)
		}
		if d == nil || path == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), "..") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return volErr(err)
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return volErr(err)
		}
		if _, err := cleanVolumeRel(rel); err != nil {
			return nil
		}
		b, err := readNoFollow(path)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || os.IsNotExist(err) {
				return nil
			}
			return volErr(err)
		}
		out[rel] = volFile{data: b, mode: info.Mode().Perm()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func readDataLink(dirFd int) (string, error) {
	var buf [255]byte
	n, err := unix.Readlinkat(dirFd, "..data", buf[:])
	if err != nil {
		return "", err
	}
	if n == len(buf) {
		return "", fmt.Errorf("volume data link is invalid")
	}
	target := string(buf[:n])
	if !validTSName(target) {
		return "", fmt.Errorf("volume data link is invalid")
	}
	return target, nil
}

func cleanPayload(files map[string]volFile) (map[string]volFile, error) {
	out := make(map[string]volFile, len(files))
	for rel, file := range files {
		c, err := cleanVolumeRel(rel)
		if err != nil {
			return nil, err
		}
		out[c] = file
	}
	for a := range out {
		for b := range out {
			if a != b && strings.HasPrefix(b, a+string(filepath.Separator)) {
				return nil, fmt.Errorf("volume file path %q conflicts with %q", a, b)
			}
		}
	}
	return out, nil
}

func cleanVolumeRel(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("empty volume file path")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("volume file path %q must be relative", name)
	}
	rel := filepath.Clean(name)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("volume file path %q escapes the volume directory", name)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "..") {
			return "", fmt.Errorf("volume file path %q escapes the volume directory", name)
		}
	}
	return rel, nil
}

func topNames(files map[string]volFile) map[string]struct{} {
	out := make(map[string]struct{}, len(files))
	sep := string(filepath.Separator)
	for rel := range files {
		name := rel
		if i := strings.Index(rel, sep); i >= 0 {
			name = rel[:i]
		}
		out[name] = struct{}{}
	}
	return out
}

func validTSName(name string) bool {
	if len(name) < 3 || !strings.HasPrefix(name, "..") || strings.ContainsAny(name, `/\`) {
		return false
	}
	switch name {
	case "..", ".", "..data", "..data_tmp":
		return false
	}
	if strings.HasPrefix(name, "..tmplink-") || strings.HasPrefix(name, "..trash-") {
		return false
	}
	return true
}

func statType(mode uint64) uint32 {
	return uint32(mode) & uint32(unix.S_IFMT)
}

func isDirRenameErr(err error) bool {
	return errors.Is(err, unix.EISDIR) || errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST)
}

func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

func writeFull(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		data = data[n:]
		if err != nil {
			return err
		}
	}
	return nil
}

func volErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("volume path is a symlink")
	}
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Err != nil {
		return fmt.Errorf("volume write: %s", pe.Err.Error())
	}
	var errno unix.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("volume write: %s", errno.Error())
	}
	if strings.ContainsAny(err.Error(), `/\`) {
		return fmt.Errorf("volume write failed")
	}
	return err
}
