package guest

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// Two jobs that boot from one image slot start on the same login keychain.
// After each job installs its own keychain, the files differ, and ending
// the first job removes its file before the second job runs.
func TestConsecutiveJobsOnOneSlotDoNotShareKeychain(t *testing.T) {
	slot := NewImageKeychains()
	base, err := slot.Default()
	if err != nil {
		t.Fatal(err)
	}
	again, err := slot.Default()
	if err != nil {
		t.Fatal(err)
	}
	if base != again || !strings.HasSuffix(base, "/login.keychain-db") {
		t.Fatalf("image login keychain = %q and %q", base, again)
	}

	a, err := InstallEphemeralKeychain(EphemeralKeychainReq{PodUID: "pod-a"}, slot)
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Delete(a.KeychainPath); err != nil {
		t.Fatal(err)
	}
	b, err := InstallEphemeralKeychain(EphemeralKeychainReq{PodUID: "pod-b"}, slot)
	if err != nil {
		t.Fatal(err)
	}
	if a.KeychainPath == b.KeychainPath {
		t.Fatalf("consecutive pods on one slot share keychain state %s", a.KeychainPath)
	}
	if !strings.HasSuffix(a.KeychainPath, "/darwin-node-job-pod-a.keychain-db") {
		t.Fatalf("pod a keychain %s", a.KeychainPath)
	}
	if !strings.HasSuffix(b.KeychainPath, "/darwin-node-job-pod-b.keychain-db") {
		t.Fatalf("pod b keychain %s", b.KeychainPath)
	}
	if slot.Exists(a.KeychainPath) {
		t.Fatal("pod a keychain still present after the job")
	}
	if !slot.Exists(b.KeychainPath) {
		t.Fatal("pod b keychain missing")
	}
	list, err := slot.UserSearchList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != b.KeychainPath {
		t.Fatalf("user search list %v", list)
	}
	def, err := slot.Default()
	if err != nil {
		t.Fatal(err)
	}
	if def != b.KeychainPath {
		t.Fatalf("default %s", def)
	}
	hidden, err := slot.LoginHidden(slot.home)
	if err != nil || !hidden {
		t.Fatalf("login hidden %v %v", hidden, err)
	}
	sys, err := slot.SystemPath()
	if err != nil || sys == "" || !slot.Exists(sys) {
		t.Fatalf("system keychain %q err %v", sys, err)
	}
	if !slot.NoAutoLock(b.KeychainPath) {
		t.Fatal("job keychain auto-locks")
	}
}

func TestInstallRejectsPodUIDBeforeAnyOSCall(t *testing.T) {
	slot := NewImageKeychains()
	for _, uid := range []string{"", "pod/a", "..", "a\nb", `pod\a`, "pod..a"} {
		if _, err := InstallEphemeralKeychain(EphemeralKeychainReq{PodUID: uid}, slot); err == nil {
			t.Fatalf("uid %q accepted", uid)
		}
	}
	if calls := slot.Calls(); len(calls) != 0 {
		t.Fatalf("calls %v", calls)
	}
}

func TestInstallStopsOnStepError(t *testing.T) {
	steps := []string{"home", "create", "unlock", "default", "search", "hide"}
	order := []string{"home", "create", "unlock", "default", "search", "hide"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			slot := NewImageKeychains()
			slot.FailAt(step)
			_, err := InstallEphemeralKeychain(EphemeralKeychainReq{PodUID: "pod-a"}, slot)
			if err == nil || !strings.Contains(err.Error(), step) {
				t.Fatalf("err %v", err)
			}
			calls := slot.Calls()
			if len(calls) == 0 || calls[len(calls)-1] != step {
				t.Fatalf("calls %v, want stop at %s", calls, step)
			}
			past := false
			seen := map[string]bool{}
			for _, c := range calls {
				seen[c] = true
			}
			for _, name := range order {
				if past && seen[name] {
					t.Fatalf("continued to %s after %s: %v", name, step, calls)
				}
				if name == step {
					past = true
				}
			}
		})
	}
}

func TestDeleteRefusesSystemAndLoginKeychains(t *testing.T) {
	slot := NewImageKeychains()
	sys, err := slot.SystemPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Delete(sys); err == nil || !slot.Exists(sys) {
		t.Fatalf("system keychain delete err %v exists %v", err, slot.Exists(sys))
	}
	login, err := slot.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Delete(login); err == nil || !slot.Exists(login) {
		t.Fatalf("login keychain delete err %v exists %v", err, slot.Exists(login))
	}
}

type fixedKeychainPassword struct {
	KeychainPlatform
	pw  string
	err error
}

func (f fixedKeychainPassword) KeychainPassword() (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.pw, nil
}

func TestKeychainPasswordStaysOffTheWire(t *testing.T) {
	const secret = "super-secret-keychain-password"
	slot := NewImageKeychains()
	res, err := InstallEphemeralKeychain(EphemeralKeychainReq{PodUID: "pod-a"}, fixedKeychainPassword{KeychainPlatform: slot, pw: secret})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	reqRaw, err := json.Marshal(EphemeralKeychainReq{PodUID: "pod-a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(reqRaw), secret) || strings.Contains(string(reqRaw), "password") {
		t.Fatalf("password on the wire: %s %s", reqRaw, raw)
	}
	if strings.Contains(res.KeychainPath, secret) || strings.Contains(res.SystemKeychain, secret) {
		t.Fatalf("password in response paths: %+v", res)
	}
	slot2 := NewImageKeychains()
	slot2.FailAt("unlock")
	_, err = InstallEphemeralKeychain(EphemeralKeychainReq{PodUID: "pod-a"}, fixedKeychainPassword{KeychainPlatform: slot2, pw: secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unlock error leaked the password: %v", err)
	}
}

func TestGuestAdvertisesEphemeralKeychain(t *testing.T) {
	if !containsCap(GuestCapabilities(), CapEphemeralKeychain) || !containsCap(HostCapabilities(), CapEphemeralKeychain) {
		t.Fatalf("host %v guest %v", HostCapabilities(), GuestCapabilities())
	}
}

func containsCap(list []string, want string) bool {
	for _, name := range list {
		if name == want {
			return true
		}
	}
	return false
}

func TestWorkloadExitDeletesJobKeychain(t *testing.T) {
	slot := NewImageKeychains()
	h := &Handler{KeychainPlatform: slot, Token: "tok"}
	h.Init()
	res, err := h.ephemeralKeychain(context.Background(), EphemeralKeychainReq{PodUID: "pod-a"})
	if err != nil {
		t.Fatal(err)
	}
	ReportProcessExit(context.Background(), h, nil, time.Millisecond, func() {})
	if slot.Exists(res.KeychainPath) {
		t.Fatal("job keychain survived workload exit")
	}
}

func TestShutdownDeletesJobKeychain(t *testing.T) {
	slot := NewImageKeychains()
	h := &Handler{KeychainPlatform: slot, Token: "tok", AgentVersion: "test"}
	h.Init()
	a, b := net.Pipe()
	defer b.Close()
	go func() { _ = Serve(context.Background(), a, *h) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := Dial(ctx, b, "tok", "test-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if !cli.HasCapability(CapEphemeralKeychain) {
		t.Fatal("session missing ephemeral-keychain")
	}
	res, err := cli.EphemeralKeychain(ctx, EphemeralKeychainReq{PodUID: "pod-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckEphemeralKeychain("pod-a", res); err != nil {
		t.Fatal(err)
	}
	if err := cli.Shutdown(ctx, ShutdownReq{Reason: "pod deleted"}); err != nil {
		t.Fatal(err)
	}
	if slot.Exists(res.KeychainPath) {
		t.Fatal("job keychain survived shutdown")
	}
	if !slot.Exists(res.SystemKeychain) {
		t.Fatal("shutdown removed the system keychain")
	}
}
