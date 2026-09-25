package digest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	ocidigest "github.com/opencontainers/go-digest"
)

const Suffix = ".digest"

// HashCount is incremented by FileSHA256. Tests use it to assert the
// sidecar fast path does not re-hash a matching file.
var HashCount atomic.Uint64

// FileSHA256 hashes the file at path.
func FileSHA256(path string) (ocidigest.Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	HashCount.Add(1)
	return ocidigest.SHA256.FromReader(f)
}

// sidecarKey binds identity lines to this process. A same-size overwrite
// changes ctime/mtime; updating those lines in the sidecar without the key
// fails the MAC, so Verify rehashes instead of trusting the stale digest.
var (
	sidecarKey   [32]byte
	sidecarKeyOK bool
)

func init() {
	_, err := rand.Read(sidecarKey[:])
	sidecarKeyOK = err == nil
}

// WriteSidecar writes path+".digest" as the digest, size, and file identity.
func WriteSidecar(path string, d ocidigest.Digest) error {
	body := d.String() + "\n"
	if st, err := os.Stat(path); err == nil {
		body = fmt.Sprintf("%s\nsize=%d\n", d.String(), st.Size())
		if ino, ctime, mtime, ok := fileID(st); ok && sidecarKeyOK {
			fp, fpErr := contentFingerprint(path, st.Size())
			if fpErr == nil {
				mac := identityMAC(d.String(), st.Size(), ino, ctime, mtime, fp)
				body += fmt.Sprintf("ino=%d\nctime=%d\nmtime=%d\nmac=%s\n", ino, ctime, mtime, mac)
			}
		}
	}
	return os.WriteFile(path+Suffix, []byte(body), 0o644)
}

func identityMAC(dig string, size int64, ino uint64, ctime, mtime int64, fp string) string {
	mac := hmac.New(sha256.New, sidecarKey[:])
	fmt.Fprintf(mac, "%s\n%d\n%d\n%d\n%d\n%s", dig, size, ino, ctime, mtime, fp)
	return hex.EncodeToString(mac.Sum(nil))
}

// contentFingerprint is a cheap sample of file bytes (first+last 4KiB) so
// same-size overwrites fail the sidecar MAC even on filesystems (overlayfs)
// that leave ctime/mtime unchanged.
func contentFingerprint(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const window = 4096
	h := sha256.New()
	buf := make([]byte, window)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return "", err
	}
	h.Write(buf[:n])
	if size > window {
		if _, err := f.Seek(size-window, io.SeekStart); err != nil {
			return "", err
		}
		n, err = f.Read(buf)
		if err != nil && err != io.EOF {
			return "", err
		}
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadSidecar reads path+".digest". Legacy (digest-only) and size-bearing
// sidecars are both accepted.
func ReadSidecar(path string) (ocidigest.Digest, error) {
	d, _, _, err := readSidecar(path)
	return d, err
}

type sidecarMeta struct {
	digest  ocidigest.Digest
	size    int64
	hasSize bool
	ino     uint64
	ctime   int64
	mtime   int64
	mac     string
	hasID   bool
}

func readSidecar(path string) (ocidigest.Digest, int64, bool, error) {
	m, err := readMeta(path)
	if err != nil {
		return "", 0, false, err
	}
	return m.digest, m.size, m.hasSize, nil
}

func readMeta(path string) (sidecarMeta, error) {
	b, err := os.ReadFile(path + Suffix)
	if err != nil {
		return sidecarMeta{}, err
	}
	return parseSidecar(b)
}

func parseSidecar(b []byte) (sidecarMeta, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return sidecarMeta{}, fmt.Errorf("empty digest sidecar")
	}
	lines := strings.Split(s, "\n")
	d, err := ocidigest.Parse(strings.TrimSpace(lines[0]))
	if err != nil {
		return sidecarMeta{}, fmt.Errorf("parse digest sidecar: %w", err)
	}
	var m sidecarMeta
	m.digest = d
	var sawIno, sawCtime, sawMtime bool
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "size":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return sidecarMeta{}, fmt.Errorf("parse digest sidecar size: %w", err)
			}
			m.size = n
			m.hasSize = true
		case "ino":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return sidecarMeta{}, fmt.Errorf("parse digest sidecar ino: %w", err)
			}
			m.ino = n
			sawIno = true
		case "ctime":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return sidecarMeta{}, fmt.Errorf("parse digest sidecar ctime: %w", err)
			}
			m.ctime = n
			sawCtime = true
		case "mtime":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return sidecarMeta{}, fmt.Errorf("parse digest sidecar mtime: %w", err)
			}
			m.mtime = n
			sawMtime = true
		case "mac":
			m.mac = val
		}
	}
	m.hasID = m.hasSize && sawIno && sawCtime && sawMtime && m.mac != ""
	return m, nil
}

// Verify checks path against expected when set (the pull-time / provenance
// digest). The sidecar is a cache, not a second source of truth: the fast
// path skips the hash only when size, inode identity, and a cheap content
// fingerprint still match, MAC'd with this process's key. Same-size
// overwrites change ctime on APFS/ext4; on overlayfs timestamps may stick,
// so the fingerprint closes that hole.
//
// Slow path: hash the file and compare to expected (or to the sidecar when
// expected is empty).
func Verify(path string, expected ocidigest.Digest) error {
	if expected != "" {
		if fastSidecarMatch(path, expected) {
			return nil
		}
		sum, err := FileSHA256(path)
		if err != nil {
			return err
		}
		if sum != expected {
			return fmt.Errorf("digest mismatch for %s: have %s want %s", path, sum, expected)
		}
		return WriteSidecar(path, sum)
	}

	sum, err := FileSHA256(path)
	if err != nil {
		return err
	}
	if got, err := ReadSidecar(path); err == nil && got != sum {
		return fmt.Errorf("digest mismatch for %s: have %s want %s", path, sum, got)
	}
	return WriteSidecar(path, sum)
}

func fastSidecarMatch(path string, expected ocidigest.Digest) bool {
	if !sidecarKeyOK {
		return false
	}
	m, err := readMeta(path)
	if err != nil || !m.hasID || m.digest != expected {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() != m.size {
		return false
	}
	ino, ctime, mtime, ok := fileID(st)
	if !ok || ino != m.ino || ctime != m.ctime || mtime != m.mtime {
		return false
	}
	fp, err := contentFingerprint(path, st.Size())
	if err != nil {
		return false
	}
	want := identityMAC(m.digest.String(), m.size, m.ino, m.ctime, m.mtime, fp)
	return hmac.Equal([]byte(want), []byte(m.mac))
}
