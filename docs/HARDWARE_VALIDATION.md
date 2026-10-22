# Hardware validation

Run this once on a real Apple Silicon Mac. It is the release gate for
Virtualization.framework: boot, guest agent, exec, logs, metrics, delete,
and the 24 hour adopt/delete soak. Linux CI and `make soak` without
`IMAGE` exercise the fake runtime only. Record the results in the sheet
at the bottom and keep the command output.

Do not start until the Mac is the machine under test (a self-hosted
runner labelled `darwin-node-e2e` is the same checklist).

## What you need

- Apple Silicon Mac (`uname -m` prints `arm64`)
- Go 1.25 or newer (`go version`)
- `codesign` on `PATH`
- A baked image directory with `disk.img`, `aux.img`, and `config.json`,
  and the guest agent injected
- Enough free disk for two macOS guests (the soak boots both slots)
- The darwin-node tree checked out at the commit you intend to ship

Bake the image once if you do not already have one:

```bash
bin/darwin-image restore --ipsw <path-to-ipsw> --out out/macos-15
bin/darwin-image inject-agent --image out/macos-15
bin/darwin-image pack --image out/macos-15 --hardware-model <model-from-restore>
```

`restore` and `inject-agent` are themselves hardware-only. A directory
produced on this Mac is what the later steps consume.

## 1. Unit tests on this Mac

```bash
make test
```

Pass: the command exits 0. This runs the Darwin `clonefile` path. It
does not boot a VM. A failure here stops the checklist.

## 2. Sign the binaries

```bash
make sign
codesign -d --entitlements :- bin/darwin-node
```

Pass: the entitlements dump contains
`com.apple.security.virtualization`. `make test-hardware` and
`make soak IMAGE=...` sign again when `codesign` is present; this step
catches a missing entitlement before a long boot.

## 3. Hardware gate

```bash
make test-hardware IMAGE=out/macos-15
```

Substitute your image directory. The Makefile builds `bin/darwin-hwtest`,
ad-hoc signs it, and runs it. With `IMAGE` unset the target prints
`SKIP` and exits 0. That skip is an unprovisioned run, so this step
always passes an image path.

Pass: exit 0, and the output contains each of these lines (the detail
after `OK` varies):

```text
STEP BOOT      OK
STEP EXEC      OK
STEP LOGS      OK
STEP METRICS   OK
STEP CONSOLE   SKIP
STEP DELETE    OK
HARDWARE GATE: PASS
```

`STEP EXEC` is a stdin/stdout round trip. The guest command prints
`hwtest-marker-42`; a missing marker fails the step. `STEP DELETE`
must leave the slot table at zero. `STEP CONSOLE` is `SKIP` unless you
opt in:

```bash
bin/darwin-hwtest --image out/macos-15 --serial-console
```

Pass for that run: `STEP CONSOLE OK` and a socket path, then
`HARDWARE GATE: PASS`.

Any `STEP ... FAIL` line, a non-zero exit, or a missing
`HARDWARE GATE: PASS` line fails the gate. The tool deletes the VM on
failure. `--keep` leaves it running and is not a pass of `STEP DELETE`.

## 4. One real soak cycle

```bash
make soak IMAGE=out/macos-15 SOAK_CYCLES=1
```

Pass: exit 0, and the log contains:

```text
SOAK start runtime=vz
SOAK PASS cycles=1
```

`runtime=vz` is required. `cap_rejects=1` on that `SOAK PASS` line means
the third create was refused while both slots were held. `SOAK FAIL`
or a non-zero exit stops the checklist. Fix that before the 24 hour run.

## 5. 24 hour soak

```bash
make soak IMAGE=out/macos-15
```

This selects the vz runtime and a 24 hour budget. Each cycle fills both
VM slots (2 vCPU and 4 GiB each, the engine default), writes a cache
marker, deletes, and checks file descriptors, pod directories, slot
occupancy, and cache-store drift. Leave the Mac awake and on AC power.
A shorter budget is a smoke test, not this step:

```bash
make soak IMAGE=out/macos-15 SOAK_DURATION=30m
```

Pass: exit 0, the process ran for 24 hours (the `elapsed=` field on
`SOAK PASS` is at least `24h0m0s`, or the run stopped because the
duration budget was reached after one or more complete cycles), and the
last lines include:

```text
SOAK PASS cycles=<n> elapsed=<at least 24h> runtime=vz max_vms=2
```

Also required on that line:

- `cap_rejects` equals `cycles` (every cycle refused one extra create)
- `fd_max` is within the harness slack of `fd_base` (the harness exits
  non-zero if it is not, so a zero exit already covers this)

`SOAK FAIL`, a signal exit, or `runtime=fake` fails the soak. The fake
runtime cannot appear on this command because `IMAGE` selects vz and a
non-Apple host refuses `--image`.

## 6. Cluster smoke

Skip this section when you are not joining a cluster. The gate and the
soak above do not need an API server.

```bash
make sign
export APISERVER_CERT_LOCATION=$HOME/.kube/tls.crt
export APISERVER_KEY_LOCATION=$HOME/.kube/tls.key
export APISERVER_CA_CERT_LOCATION=$HOME/.kube/ca.crt
./bin/darwin-node --nodename=e2e-mac --allow-nat-workloads
```

In another shell:

```bash
kubectl apply -f examples/pod.yaml
kubectl wait --for=condition=Ready pod/macos --timeout=15m
kubectl exec macos -c macos -- /usr/bin/uname -a
kubectl logs macos -c macos --tail=50
kubectl apply -f examples/pod-hybrid.yaml
```

Pass:

- `kubectl get node` shows the node Ready
- `pod/macos` reaches Ready within 15 minutes
- `uname` prints a Darwin kernel
- `kubectl logs` returns guest output
- A third pod that requests a VM slot becomes Failed with
  `VMCapacityExhausted`

## Result sheet

Copy this into the PR or the run log and fill it in.

```text
Date:
Commit:
Host (sysctl -n machdep.cpu.brand_string):
macOS (sw_vers -productVersion):
Image directory:
make test:                 PASS / FAIL
make sign entitlements:    PASS / FAIL
make test-hardware:        PASS / FAIL
soak SOAK_CYCLES=1:        PASS / FAIL
soak 24h:                  PASS / FAIL   (cycles=, elapsed=, cap_rejects=)
cluster smoke:             PASS / FAIL / skipped
```

## Already covered without this Mac

These stay in Linux CI. They do not check a box above.

- `make test-short` and `go test ./... -short` (fake runtime, unit tests)
- `make soak` with no `IMAGE` (4 fake-runtime cycles)
- `make test-kubelet-auth` (TokenReview and SubjectAccessReview)

Host-side `emptyDir` size limits and projected ServiceAccount token
refresh are also covered by those unit tests. An APFS quota that
rejects the write itself is not part of this checklist.
