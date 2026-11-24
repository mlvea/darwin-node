package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/digest"
	"github.com/darwin-node/darwin-node/internal/disk"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// A delta layer may declare an uncompressed size of 1 PiB. The pull then
// truncates the disk to that size and hashes the hole. Pod delete cannot
// stop a hash that never looks at the context.
func TestFindDeltaLayerRejectsOnePiBDeclaredSize(t *testing.T) {
	const onePiB = int64(1) << 50
	hex64 := strings.Repeat("ab", 32)
	_, found, err := findDeltaLayer([]ocispec.Descriptor{{
		MediaType: MediaTypeDiskDelta,
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "disk.delta.patch",
			AnnotDeltaBaseRef:       "example.com/base:v1",
			AnnotDeltaBaseDigest:    hex64,
			AnnotUncompressedDigest: "sha256:" + hex64,
			AnnotUncompressedSize:   strconv.FormatInt(onePiB, 10),
		},
	}})
	if !found {
		t.Fatal("descriptor is a delta layer")
	}
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("1 PiB declared size was accepted: %v", err)
	}
}

func TestApplyDeltaRejectsOnePiBDestination(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	disk := []byte("base-disk")
	writeImageDirWithDisk(t, base, disk)
	sum, err := digest.FileSHA256(filepath.Join(base, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	deltaDir := t.TempDir()
	const onePiB = int64(1) << 50
	if err := WriteDeltaManifest(deltaDir, DeltaManifest{Patches: []PatchRef{{
		Name:     "disk.img",
		BaseSHA:  sum.String(),
		BaseSize: int64(len(disk)),
		DestSHA:  "sha256:" + strings.Repeat("cd", 32),
		DestSize: onePiB,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deltaDir, "disk.img.patch"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- ApplyDelta(base, deltaDir, filepath.Join(t.TempDir(), "out"))
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("1 PiB destination was not rejected by a size limit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ApplyDelta did not reject a 1 PiB destination before hashing it")
	}
}

func TestPullDeltaSelfReferenceDoesNotHang(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()
	deltaRef := reg.refFor("team/app", "self")
	pushDeltaLayer(t, reg, deltaRef, deltaRef, strings.Repeat("ab", 32), strings.Repeat("cd", 32), 64, []byte("patch"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := NewManager(t.TempDir()).Pull(ctx, deltaRef, RegistryCreds{}, false)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("self-base pull ran for %s before returning %v", elapsed, err)
	}
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("got %v", err)
	}
}

func TestPullDeltaCycleDoesNotHang(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()
	refA := reg.refFor("team/app", "a")
	refB := reg.refFor("team/app", "b")
	pushDeltaLayer(t, reg, refA, refB, strings.Repeat("ab", 32), strings.Repeat("cd", 32), 64, []byte("patch-a"))
	pushDeltaLayer(t, reg, refB, refA, strings.Repeat("cd", 32), strings.Repeat("ab", 32), 64, []byte("patch-b"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := NewManager(t.TempDir()).Pull(ctx, refA, RegistryCreds{}, false)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("cyclic delta pull ran for %s before returning %v", elapsed, err)
	}
	if err == nil || !deltaBaseRejected(err) {
		t.Fatalf("got %v", err)
	}
}

func TestPullDeltaOfDeltaIsRejected(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()

	baseDisk := patternDisk(64<<10, 3)
	baseRef, _ := publishBase(t, reg, "team/base", "v1", baseDisk)

	midDisk := append([]byte(nil), baseDisk...)
	midDisk[100] ^= 0xff
	midRef := publishDelta(t, reg, "team/mid", "v1", baseRef, baseDisk, midDisk)

	topDisk := append([]byte(nil), midDisk...)
	topDisk[200] ^= 0xff
	topRef := publishDelta(t, reg, "team/top", "v1", midRef, midDisk, topDisk)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := NewManager(t.TempDir()).Pull(ctx, topRef, RegistryCreds{}, false)
	if err == nil || !deltaBaseRejected(err) {
		t.Fatalf("delta-of-delta pull returned %v", err)
	}
}

func deltaBaseRejected(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "itself a delta") || strings.Contains(msg, "cycle")
}

func publishDelta(t *testing.T, reg *casRegistry, repo, tag, baseRef string, baseDisk, targetDisk []byte) string {
	t.Helper()
	deltaDir := t.TempDir()
	CreatePatchSilent(t, baseDisk, targetDisk, deltaDir)
	patch, err := os.ReadFile(filepath.Join(deltaDir, "disk.img.patch"))
	if err != nil {
		t.Fatal(err)
	}
	ref := reg.refFor(repo, tag)
	pushDeltaLayer(t, reg, ref, baseRef, digestOf(baseDisk), digestOf(targetDisk), int64(len(targetDisk)), patch)
	return ref
}

func pushDeltaLayer(t *testing.T, reg *casRegistry, ref, baseRef, baseHex, destHex string, destSize int64, patch []byte) {
	t.Helper()
	pushArtifact(t, reg, ref, testConfigJSON(t, "delta"), []layerSpec{{
		mediaType: MediaTypeDiskDelta,
		title:     "disk.delta.patch",
		data:      patch,
		ann: map[string]string{
			AnnotDeltaBaseRef:       baseRef,
			AnnotDeltaBaseDigest:    baseHex,
			AnnotUncompressedDigest: "sha256:" + destHex,
			AnnotUncompressedSize:   fmt.Sprint(destSize),
		},
	}})
}

// Cancelling the pod that started a shared pull must not fail the other
// pods waiting on the same image. The registry request stays open until
// every waiter has gone.
func TestSharedPullSurvivesLeaderCancel(t *testing.T) {
	reg := newCASRegistry()
	disk := []byte("shared-disk-bytes")
	ref, _ := publishBase(t, reg, "team/macos", "v1", disk)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	// Release the handler before closing the server. A failing assertion
	// would otherwise leave the blob GET blocked and Close would wait.
	defer func() {
		releaseFn()
		reg.close()
	}()

	started := make(chan struct{})
	var startOnce sync.Once
	var reqMu sync.Mutex
	var blobReq *http.Request
	reg.onBlob = func(r *http.Request) {
		reqMu.Lock()
		if blobReq == nil {
			blobReq = r
		}
		reqMu.Unlock()
		startOnce.Do(func() { close(started) })
		<-release
	}

	m := NewManager(t.TempDir())
	ctx1, cancel1 := context.WithCancel(context.Background())
	err1c := make(chan error, 1)
	go func() {
		_, err := m.Pull(ctx1, ref, RegistryCreds{}, true)
		err1c <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pull never requested a blob")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	type pullResult struct {
		img LocalImage
		err error
	}
	err2c := make(chan pullResult, 1)
	go func() {
		img, err := m.Pull(ctx2, ref, RegistryCreds{}, true)
		err2c <- pullResult{img, err}
	}()
	waitForPullFrames(t, 2)
	cancel1()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		reqMu.Lock()
		err := blobReq.Context().Err()
		reqMu.Unlock()
		if err != nil {
			t.Fatalf("leader cancel aborted the shared pull: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	releaseFn()

	select {
	case res := <-err2c:
		if res.err != nil {
			t.Fatalf("second waiter failed after the leader was cancelled: %v", res.err)
		}
		got, err := os.ReadFile(res.img.DiskPath)
		if err != nil || string(got) != string(disk) {
			t.Fatalf("disk %q err=%v", got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second waiter did not finish")
	}
	select {
	case err := <-err1c:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader error %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not return")
	}
}

// The shared pull is cancelled once nobody is waiting. A deleted pod must
// not leave the registry read running and holding its VM slot.
func TestLastWaiterCancelsSharedPull(t *testing.T) {
	reg := newCASRegistry()
	disk := []byte("only-waiter")
	ref, _ := publishBase(t, reg, "team/macos", "only", disk)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		releaseFn()
		reg.close()
	}()

	started := make(chan struct{})
	var startOnce sync.Once
	var reqMu sync.Mutex
	var blobReq *http.Request
	reg.onBlob = func(r *http.Request) {
		reqMu.Lock()
		if blobReq == nil {
			blobReq = r
		}
		reqMu.Unlock()
		startOnce.Do(func() { close(started) })
		<-release
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := NewManager(t.TempDir()).Pull(ctx, ref, RegistryCreds{}, true)
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pull never requested a blob")
	}
	cancel()
	reqMu.Lock()
	reqCtx := blobReq.Context()
	reqMu.Unlock()
	select {
	case <-reqCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the only waiter left the registry read running")
	}
	releaseFn()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pull did not return after the last waiter left")
	}
}

func TestPullDeadlineBoundsHungRegistry(t *testing.T) {
	ref, started := hungBlob(t, "hung", []byte("disk-bytes-hung"))
	m := NewManager(t.TempDir())
	m.PullTimeout = 200 * time.Millisecond
	m.ReadStall = time.Hour
	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := m.Pull(context.Background(), ref, RegistryCreds{}, true)
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pull never requested a blob")
	}
	select {
	case err := <-errc:
		if time.Since(start) > 2*time.Second {
			t.Fatalf("pull deadline did not fire after %s: %v", time.Since(start), err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hung pull did not return")
	}
}

func TestPullReadStallFailsQuietRegistry(t *testing.T) {
	ref, started := hungBlob(t, "stall", []byte("disk-bytes-stall"))
	m := NewManager(t.TempDir())
	m.PullTimeout = time.Minute
	m.ReadStall = 200 * time.Millisecond
	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := m.Pull(context.Background(), ref, RegistryCreds{}, true)
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pull never requested a blob")
	}
	select {
	case err := <-errc:
		if time.Since(start) > 2*time.Second {
			t.Fatalf("read stall was not enforced after %s: %v", time.Since(start), err)
		}
		if err == nil || !strings.Contains(err.Error(), "stall") {
			t.Fatalf("got %v, want a read stall", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled pull did not return")
	}
}

func TestPullRejectsGzipExpansionOverLimit(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()
	payload := bytes.Repeat([]byte("z"), 64<<10)
	ref := reg.refFor("team/macos", "gzip")
	pushArtifact(t, reg, ref, testConfigJSON(t, "gzip"), []layerSpec{
		{mediaType: MediaTypeDisk, title: "disk.img", data: gzipBytes(t, payload)},
		{mediaType: MediaTypeAux, title: "aux.img", data: []byte("AUX")},
	})
	m := NewManager(t.TempDir())
	m.MaxImageBytes = 8 << 10
	_, err := m.Pull(context.Background(), ref, RegistryCreds{}, true)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("gzip expansion over the limit returned %v", err)
	}
}

func TestPullRejectsCompressedBlobOverLimit(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()
	ref, _ := publishBase(t, reg, "team/macos", "fat", bytes.Repeat([]byte("d"), 20<<10))
	m := NewManager(t.TempDir())
	m.MaxCompressedBytes = 2 << 10
	_, err := m.Pull(context.Background(), ref, RegistryCreds{}, true)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized compressed blob returned %v", err)
	}
	if _, statErr := os.Stat(CacheDir(m.CacheDir, ref)); !os.IsNotExist(statErr) {
		t.Fatalf("rejected pull published a cache entry: %v", statErr)
	}
}

func TestPullRejectsConfiguredDeclaredSize(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()
	ref := reg.refFor("team/app", "big")
	pushDeltaLayer(t, reg, ref, reg.refFor("team/base", "v1"), strings.Repeat("ab", 32), strings.Repeat("cd", 32), 4096, []byte("patch"))
	m := NewManager(t.TempDir())
	m.MaxImageBytes = 1024
	_, err := m.Pull(context.Background(), ref, RegistryCreds{}, true)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("configured declared-size limit returned %v", err)
	}
}

func TestPullRejectsWhenHeadroomExceedsFreeSpace(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()
	ref, _ := publishBase(t, reg, "team/macos", "room", []byte("small-disk"))
	orig := disk.Available
	disk.Available = func(string) (int64, error) { return 50, nil }
	t.Cleanup(func() { disk.Available = orig })
	m := NewManager(t.TempDir())
	m.DiskHeadroom = 1000
	_, err := m.Pull(context.Background(), ref, RegistryCreds{}, true)
	if err == nil || !strings.Contains(err.Error(), "headroom") {
		t.Fatalf("got %v, want a headroom error", err)
	}
}

func TestHashReaderStopsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var reads int
	r := readerFunc(func(p []byte) (int, error) {
		reads++
		if reads == 2 {
			cancel()
		}
		if reads > 2 {
			return 0, fmt.Errorf("read %d times after cancel", reads)
		}
		if len(p) > 32 {
			p = p[:32]
		}
		for i := range p {
			p[i] = 'a'
		}
		return len(p), nil
	})
	_, err := hashReader(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v after %d reads, want context.Canceled", err, reads)
	}
	if reads != 2 {
		t.Fatalf("reads=%d, hash loop did not stop at cancel", reads)
	}
}

func TestApplyDeltaStopsWhenContextCancelled(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	disk := patternDisk(4096, 4)
	writeImageDirWithDisk(t, base, disk)
	target := append([]byte(nil), disk...)
	target[10] ^= 0xff
	deltaDir := t.TempDir()
	CreatePatchSilent(t, disk, target, deltaDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := applyDelta(ctx, base, deltaDir, filepath.Join(t.TempDir(), "out"), defaultLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func hungBlob(t *testing.T, tag string, disk []byte) (string, <-chan struct{}) {
	t.Helper()
	reg := newCASRegistry()
	ref, _ := publishBase(t, reg, "team/macos", tag, disk)
	release := make(chan struct{})
	var once sync.Once
	started := make(chan struct{})
	var startOnce sync.Once
	reg.onBlob = func(*http.Request) {
		startOnce.Do(func() { close(started) })
		<-release
	}
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		reg.close()
	})
	return ref, started
}

func gzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func waitForPullFrames(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		// waitFlight runs only after the waiter has joined the flight.
		// Counting Pull is too early: the second caller is inside Pull
		// before it increments the waiter count, and cancelling then
		// still looks like the last waiter.
		if strings.Count(string(buf[:n]), "image.(*Manager).waitFlight") >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wanted %d waitFlight frames", want)
}
