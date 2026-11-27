//go:build linux || darwin

package supervised

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSpawnReapsChild(t *testing.T) {
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	p, err := spawnProcess(bin, nil, filepath.Join(t.TempDir(), "true.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Kill)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("child was not reaped")
	}
	if err := unix.Kill(p.cmd.Process.Pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("reaped child still signalable: %v", err)
	}
}

func TestSpawnFileStdioNotPipes(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "report")
	logPath := filepath.Join(dir, "supervisor.log")
	t.Setenv(envStdioProbe, "1")
	t.Setenv(envStdioProbeOut, out)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := spawnProcess(exe, nil, logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Kill)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stdio probe did not exit")
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var sid, pid int
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("report line %q\n%s", line, body)
		}
		switch fields[0] {
		case "sid":
			sid, err = strconv.Atoi(fields[1])
			if err != nil {
				t.Fatal(err)
			}
		case "pid":
			pid, err = strconv.Atoi(fields[1])
			if err != nil {
				t.Fatal(err)
			}
		default:
			got[fields[0]] = fields[1]
		}
	}
	var problems []string
	if got["0"] != "char" {
		problems = append(problems, fmt.Sprintf("stdin %s, want char (/dev/null)", got["0"]))
	}
	if got["1"] != "file" {
		problems = append(problems, fmt.Sprintf("stdout %s, want a log file", got["1"]))
	}
	if got["2"] != "file" {
		problems = append(problems, fmt.Sprintf("stderr %s, want a log file", got["2"]))
	}
	if sid == 0 || sid != pid {
		problems = append(problems, fmt.Sprintf("session %d pid %d, want a new session", sid, pid))
	}
	if len(problems) > 0 {
		t.Fatalf("%s\n%s", strings.Join(problems, "; "), body)
	}
}

func reportStdioProbe() error {
	out := os.Getenv(envStdioProbeOut)
	if out == "" {
		return fmt.Errorf("stdio probe output path is empty")
	}
	var b strings.Builder
	for _, fd := range []int{0, 1, 2} {
		kind, err := fdKind(fd)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%d %s\n", fd, kind)
	}
	sid, err := unix.Getsid(0)
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "sid %d\n", sid)
	fmt.Fprintf(&b, "pid %d\n", os.Getpid())
	return os.WriteFile(out, []byte(b.String()), 0o600)
}

func fdKind(fd int) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFIFO:
		return "fifo", nil
	case unix.S_IFREG:
		return "file", nil
	case unix.S_IFCHR:
		return "char", nil
	case unix.S_IFSOCK:
		return "sock", nil
	default:
		return "other", nil
	}
}
