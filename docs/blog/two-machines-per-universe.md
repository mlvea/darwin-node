# Two machines per universe

### Building a Kubernetes node out of virtualized Macs, at the edge of what Apple permits

*An end-to-end architecture tour of `darwin-node`.*

---

Kubernetes has a foundational assumption so deep nobody states it anymore:
**capacity is fungible**. A node advertises `pods: 110`, and the scheduler
spends that budget one pod at a time, trusting that pod #111 will fit tomorrow
if today's workloads die. Every scheduler extension, every autoscaler, every
bin-packing heuristic rests on the idea that the number in
`status.capacity` is a soft economic fact rather than a physical law.

On an Apple Silicon Mac running macOS workloads, it is a physical law.
XNU's hypervisor enforces a quota of **two concurrent Virtualization.framework
VMs per host**, and Apple's macOS Software License Agreement agrees with the
kernel. Not "two is recommended." Two, or the third `VM_START` fails.

This post is the architecture tour of `darwin-node`: a Kubernetes node agent
that treats that constraint not as an obstacle but as the design's center of
gravity. It runs *native macOS virtual machines as pods* — full kernels,
Metal GPUs, Xcode toolchains — alongside Linux sidecar containers, and it
joins an ordinary cluster as an ordinary node via Virtual Kubelet.

Everything interesting in the system falls out of the number two. Capacity
must be admitted, not scheduled. Images are entire operating systems, so the
storage story matters more than the network story. And since each pod contains
a whole computer with its own kernel, we had to build a control plane *inside*
the thing Kubernetes thinks is a container.

Here is the whole system in one picture. Everything after this is zooming in.

```
                        ┌──────────────────────────┐
                        │   Kubernetes API server  │
                        └────────────┬─────────────┘
                                     │ watches, heartbeats, mTLS
                                     ▼
┌─────────────────────────────────────────────────────────────────────┐
│  darwin-node  (one Go binary, the "kubelet")                        │
│                                                                     │
│  ┌──────────────┐   ┌──────────────┐   ┌──────────────────────────┐ │
│  │ Virtual      │──▶│ Pod engine   │──▶│ VZ runtime               │ │
│  │ Kubelet      │   │ lifecycle,   │   │ Virtualization.framework │ │
│  │ provider     │   │ probes, hooks│   │ boot · vsock · virtio-fs │ │
│  └──────┬───────┘   └──────┬───────┘   └──────────┬───────────────┘ │
│         │                  │                      │                 │
│  ┌──────▼───────┐   ┌──────▼───────┐   ┌──────────▼───────────────┐ │
│  │ Slot table   │   │ Image store  │   │        ┌───────────┐     │ │
│  │ 2 slots.     │   │ OCI pull ·   │   └────────│  VM #1    │     │ │
│  │ Fail closed. │   │ verify · CoW │            └───────────┘     │ │
│  └──────────────┘   └──────────────┘            ┌───────────┐     │ │
│                                                 │  VM #2    │     │ │
│  ┌──────────────┐                               └─────┬─────┘     │ │
│  │ Docker       │  Linux sidecars                     │           │
│  │ sidecars     │◀────────────────────────────────────┼───────┐   │
│  └──────────────┘                                     │       │   │
└───────────────────────────────────────────────────────┼───────┼───┘
                                   vsock (point-to-point)│       │ Docker API
                                                        ▼       ▼
                                             ┌──────────────┐ ┌─────────┐
                                             │ guest agent  │ │ colima/ │
                                             │ root in-guest│ │ Desktop │
                                             └──────────────┘ └─────────┘
```

---

## Table of contents

- [1. The constraint](#1-the-constraint)
- [2. Admission: capacity you cannot negotiate with](#2-admission)
- [3. Shipping operating systems as OCI artifacts](#3-images)
- [4. Copy-on-write for free](#4-cow)
- [5. A control plane inside every VM](#5-agent)
- [6. The boot path, timed](#6-boot)
- [7. Hybrid pods: one foot in each world](#7-hybrid)
- [8. Networking without a CNI](#8-networking)
- [9. Trust boundaries](#9-trust)
- [10. Telling the truth to the scheduler](#10-truth)
- [11. What we got wrong](#11-wrong)
- [12. The philosophy](#12-philosophy)

---

## 1. The constraint <a name="1-the-constraint"></a>

Run `docker run` on Linux and you fork a process tree inside some cgroups.
Run a pod on a normal kubelet and the marginal cost of one more pod is a few
megabytes of overhead and some kernel objects. This is why Kubernetes works.

A macOS application workload cannot live in a container. There is no
`runc` for Darwin; there are no namespaces; there is no common kernel to
share, because the *only* sanctioned way to run macOS binaries on your Mac
is to boot macOS. On Apple Silicon the sanctioned way to do *that* is
Virtualization.framework, which boots a full guest kernel with paravirtual
virtio devices — and which the hypervisor will only let you start twice.

So the unit of scheduling here is not a container. It is a *computer*.
That changes almost everything:

| | Normal kubelet node | darwin-node |
|---|---|---|
| Pod isolation | namespaces + cgroups | hardware virtualization |
| Marginal pod cost | MBs, milliseconds | GBs of RAM, ~30–60s of boot |
| Capacity ceiling | soft, tunable | **hard, in the kernel** |
| Image | layers of tar, shared kernel | an entire disk image, own kernel |
| exec/logs/probes | nsenter / cgroup fds | a protocol to a daemon in the guest |

The last row is the sneaky one. On a normal node, `kubectl exec` is the
kubelet reaching into its own kernel. Here, the kubelet must *talk to another
operating system* over a device bus, and whatever speaks on the far end is
part of the product. We'll get there.

First, the discipline question: how do you let a scheduler — which is
optimized to overcommit — loose on a machine where the third VM is illegal?

---

## 2. Admission: capacity you cannot negotiate with <a name="2-admission"></a>

The naive approach is to advertise `pods: 110` and let the kubelet reject
overflow. That produces the worst kind of failure: the scheduler believes a
lie, ships workloads to a node that cannot run them, and the error surfaces
as flapping `FailedScheduling` events minutes later, on fire, in production.

Our rule is that **a lie must be impossible to tell**. The slot table is the
only component allowed to decide whether a VM may exist, it is tiny enough to
hold in your head, and it fails closed:

```go
// TryAcquire takes a slot for uid. Never blocks.
// A third distinct uid fails with ErrVMCapacityExhausted.
func (s *Slots) TryAcquire(uid string) error {
	if uid == "" {
		return fmt.Errorf("empty pod uid") // anonymous capacity does not exist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[uid]; ok {
		return nil                             // idempotent re-admission
	}
	if len(s.held) >= s.max {
		return ErrVMCapacityExhausted          // the law, stated plainly
	}
	s.held[uid] = struct{}{}
	return nil
}
```

Three properties matter more than the code. Admission happens **before**
any expensive work — image pulls, volume materialization, VM construction —
so a malformed pod burns zero resources. Release is idempotent, because
delete paths race and double-release must be harmless. And the table keys on
**pod UID**, not name, because Kubernetes guarantees a recreated pod gets a
new identity even when it reuses a name.

But refusing pods is only half of honesty. The other half is *telling the
scheduler to stop asking*. The node publishes a custom condition and taint
that track the slot table, refreshed on a 15-second heartbeat:

```
        slot acquired                                slot released
             │                                            ▲
             ▼                                            │
   ┌─────────────────┐   both slots held   ┌────────────────────────┐
   │ slots.Full() ?  ├────────────────────▶│ taint:                 │
   └────────┬────────┘                     │ darwin.node/vm-full    │
            │ no                           │ =true : NoSchedule     │
            ▼                              │ condition VMCapacity=  │
   scheduler keeps                         │ True                   │
   sending pods;                           └───────────┬────────────┘
   engine admits                                       │ 15s heartbeat
   them normally                                       ▼
                                             node object patched;
                                             new pods route elsewhere
```

With two slots this sounds like overkill — you could almost count on your
fingers. But the mechanism is the point: the same loop carries memory
pressure, disk pressure, and Metal availability. A node whose status is
recomputed from reality on a timer is a node you can reason about at 3am.
One whose status was computed once at boot is a museum exhibit.

Allocatable follows the same honesty rule. The node reports host CPU/memory
minus explicit reservations (defaults: 2 cores, 4 GiB, 20 GiB of disk), and
advertises `pods: 2` — equal to the VM count, because that is the truth,
even though it makes this the smallest capacity node in any cluster it joins.

---

## 3. Shipping operating systems as OCI artifacts <a name="3-images"></a>

A macOS VM image is not an app. It is a **bootable disk** — tens of gigabytes
of APSW-formatted filesystem containing a kernel, frameworks, and whatever
toolchains the tenant needs — plus a small *auxiliary* storage file where the
guest stores its machine identity and NVRAM variables, plus a hardware-model
blob that describes which Mac this disk believes it lives in.

We ship these as OCI artifacts anyway. Not Docker images — OCI *artifacts*,
pushed and pulled with ordinary registries, using ORAS:

```
registry.example.com/ci/sonoma-xcode15:14.3.1
│
│   manifest (OCI)
├─── application/vnd.darwin-node.config.v1+json     config.json   (~1 KiB)
│      hardwareModelData, machineIdData, storage roles, annotations
├─── application/vnd.darwin-node.disk.v1            disk.img      (gzip, ~10⁰¹ B)
└─── application/vnd.darwin-node.aux.v1             aux.img       (gzip, ~MiB)
```

Using OCI buys us the boring superpowers for free: auth, proxies, mirrors,
air-gapped bundling, retention policies, and the fact that every registry
operator on Earth already knows how to store these blobs. The price is that
registries speak *tarball-and-manifest*, not *hypervisor-ready-disk*, so the
puller has to rebuild a bootable artifact from blobs. Three problems fall out
of that, and each got a dedicated mechanism.

**Problem one: which blob is which?** A registry will happily serve us five
layers with unhelpful names. We identify layers by media type first, exact
title second — never substring heuristics. If neither identifies a disk, an
aux, and a config, the pull *fails*: a partial image is worse than no image,
because it fails forty seconds later in a place that looks like a bug in
something else.

**Problem two: decompressing tens of gigabytes shouldn't allocate tens of
gigabytes.** Disk images compress beautifully — most of any macOS volume is
zeros — so the transformation is gzip-in, sparse-file-out. We stream the
inflater through a writer that skips all-zero megabyte pages entirely,
leaving actual holes in the output file that APFS doesn't materialize:

```go
const sparseBlock = 1 << 20 // 1 MiB

for {
	n, err := io.ReadFull(r, buf)          // buf: one reusable 1 MiB page
	if n > 0 {
		if !bytes.Equal(buf[:n], zeroBlock[:n]) {  // all zeros?
			f.WriteAt(buf[:n], off)        // then write nothing at all
		}
		off += int64(n)
	}
	...
}
f.Truncate(off)   // trailing zeros become length, not data
```

The result: the file's *logical size* matches the image bit-for-bit (the
hypervisor cares), while its *physical footprint* is roughly the compressed
size. A 64 GiB image typically lands in single-digit GiB of real blocks.

**Problem three: how do you know the disk you booted yesterday is the disk
you pulled?** Registries verify compressed-blob digests in transit; we go
further and anchor expectations at pull time. As each descriptor arrives we
record its *uncompressed* digest and size (from OCI annotations) into a
`provenance.json` lockfile next to the materialized image:

```
cache/images/sonoma-xcode15/
├── config.json          ← verified against provenance["config.json"]
├── disk.img             ← sha256 must equal provenance.files["disk.img"]
│                          size    must equal provenance.sizes["disk.img"]
├── aux.img              ← ditto
├── provenance.json      ← written once at pull time, 0600
└── *.digest             ← content-addressed fast-path cache (digest+size)
```

Verification on later pod starts is then O(1): if the sidecar records
`(expected digest, expected size)` and both match the file on disk, no hashing
happens at all. Only when provenance disagrees — or someone touched the file —
do we pay for a full SHA-256 pass. Integrity anchored to *content and
expectations*, never to mtimes; mtimes are suggestions, digests are facts.

And the whole cache directory is `0700`, because a bootable OS image is not
just data. It is code, waiting to be run.

---

## 4. Copy-on-write for free <a name="4-cow"></a>

Here is where the Mac pays rent. Every pod start clones the golden image:

```
            cache/images/sonoma-xcode15/disk.img
            (golden, immutable, ~8 GiB resident)
                        │
        clonefile()     │   APFS copies the inode, not the bytes.
        ┌───────────────┼───────────────────┬───────────────┐
        ▼               ▼                   ▼               ▼
 pods/<uid-A>/disk  pods/<uid-B>/disk  pods/<uid-C>/disk  ...
        │               │                   │
     writes land    writes land        writes land
     in new blocks  in new blocks      in new blocks
```

`clonefile(2)` is a metadata operation: the clone shares every extent with
its origin until one of them writes, at which point APFS breaks that extent
off. Cost per pod start: two syscalls and microseconds. Cost per pod in
blocks: whatever the guest actually dirties. With a two-slot host this means
image churn is essentially free and cold-starts are bounded by VM boot, not
by copying tens of gigabytes.

It also gives us crash semantics for free. The golden image is opened by
nobody; pods only ever touch their own clones; a killed VM leaves an orphaned
overlay that a 24-hour sweep collects at boot. The invariant is simple to
state: **goldens are immutable, overlays are owned by one UID, and nothing
else exists.**

---

## 5. A control plane inside every VM <a name="5-agent"></a>

Once the VM boots, the host needs to reach inside it: exec, logs, probes,
network info, volume placement, graceful shutdown. On a normal node this is
`nsenter`. Here it is a wire protocol, and we had opinions about the wire.

The transport hierarchy is short and strict:

```
  ┌──────────────────────────────────────────────────────────────┐
  │ 1. vsock        device-bus socket, host↔guest, point-to-point│  preferred
  │                 not routed, not reachable from other VMs     │
  ├──────────────────────────────────────────────────────────────┤
  │ 2. TCP          plaintext, opt-in (--agent-tcp-fallback),    │  escape hatch
  │                 off by default, token+nonce gated            │
  ├──────────────────────────────────────────────────────────────┤
  │ 3. SSH          opt-in, key-only, pinned host key, argv-     │  last resort
  │                 quoted, for images without the agent baked in│
  └──────────────────────────────────────────────────────────────┘
```

vsock deserves a sentence of appreciation. It is a socket address family
whose "address" is a *CID assigned by the hypervisor* — the guest dials CID 2
(host), the host accepts on the VM's socket device. No IP stack involved, no
ARP, no routing, nothing for a neighbor VM to touch. When your threat model
includes "the workload is a full computer that someone else's YAML asked for"
— and on this node it does — a transport that physically bypasses the network
is worth designing everything else around.

Over whichever transport, flows a framed JSON-RPC-ish protocol. One envelope,
length-prefixed:

```
 ┌─────────┬──────────┬───────┬────────┬───────────┬─────────────┐
 │ u32 len │ v = 1    │ id    │ kind   │ method    │ payload     │
 │ 4 bytes │ protocol │ uint  │ req/res│ Exec      │ json.RawMsg │
 │ big-end │ version  │       │ /stream│ Probe ... │  ≤ 16 MiB   │
 │ /error  │          │       │        │           │             │
 └─────────┴──────────┴───────┴────────┴───────────┴─────────────┘
```

The `id` field turns one connection into a multiplexer: requests carry an ID,
responses carry it back, and the client demuxes into per-call channels. That
single decision lets probes, health checks, and exec streams share one vsock
without head-of-line blocking — provided the *server* also refuses to run
handlers serially, which it does: every post-handshake request is dispatched
to its own goroutine behind a small in-flight semaphore, and slow consumers
get their stream dropped with a `consumer_too_slow` error instead of stalling
the reader.

Authentication is one handshake, and it fails closed in every direction:

```go
func authorizeHandshake(h Handler, req HandshakeReq) *CallError {
	token := h.Token                       // loaded from control share,
	if h.live != nil {                     // hot-reloaded every 2s
		token = h.live.getToken()
	}
	if h.AllowInsecureNoToken {            // explicit dev escape hatch only
		if token != "" && req.Token != token {
			return unauthorized("bad token")
		}
		return nil
	}
	if token == "" || req.Token != token { // empty configured token ⇒
		return unauthorized("bad token")   // EVERY handshake fails.
	}                                      // Fail closed means fail closed.
	return nil
}
```

Note the middle case, because it is the whole security posture in six lines.
The original sin of this codebase's ancestor was `if h.Token != "" && req.Token
!= h.Token` — an empty token accepted everyone. The fix wasn't adding a check;
it was *inverting the default*. An agent that cannot prove which pod it
belongs to serves nothing, and the TCP listener doesn't even open until a
non-empty token appears on the control share. A missing virtio-fs mount
degrades to "unreachable," never to "unauthenticated."

The per-pod token itself is 32 random bytes generated at admission, written
`0600` onto a read-only control share the guest mounts over virtio-fs, and
accompanied by a fresh 128-bit nonce per connection checked against a
4096-entry replay window. Resource limits ride along in the same handshake
path — 8 connections, 4 concurrent execs, 256 KiB per log line, 8 MiB log
ring, 1 MiB exec capture — because a guest workload that floods its own
control plane should degrade its own telemetry, not the node's.

What runs on the far end is deliberately boring: a launchd daemon, root
inside the *guest* (which is not the host), executing exactly the verbs the
Kubernetes API already authorizes — exec argv, probe handlers, tail/follow
logs, `NetInfo`, volume placement, shutdown. No shell parsing anywhere; argv
crosses the wire as argv. Same trust model as a kubelet, one layer of glass
away.

---

## 6. The boot path, timed <a name="6-boot"></a>

Pod creation is where every subsystem meets, so it deserves a timeline. All
numbers below are real configuration values from the code:

```
kubectl apply ──▶ API server ──▶ informer ──▶ CreatePod
                                                    │
  t=0          ┌────────────────────────────────────┘
               ▼
  ┌─────────────────────────┐
  │ ADMISSION               │  validate spec · resolve ConfigMaps/Secrets/
  │ (fast, synchronous)     │  SA tokens · imagePullSecrets · hostPath
  │                         │  allowlist · slot TryAcquire · hostPort
  │                         │  reservation   ← all BEFORE anything expensive
  └────────────┬────────────┘
               ▼  event: Created
  ┌─────────────────────────┐
  │ MATERIALIZE (async)     │  volumes → cache/pods/<uid>/volumes
  │                         │  token  → cache/pods/<uid>/control  (0600)
  │                         │  init containers run to completion
  └────────────┬────────────┘
               ▼  event: Starting
  ┌─────────────────────────┐
  │ IMAGE                   │  cache hit? verify (O(1) fast path)
  │                         │  miss?  ORAS pull → sparse-gunzip → provenance
  │                         │  then clonefile → pods/<uid>/overlay
  └────────────┬────────────┘
               ▼
  ┌─────────────────────────┐
  │ VM BOOT                 │  VZ config (CPU/mem from pod requests,
  │                         │  MAC from per-UID lease, virtio-fs shares,
  │        30–60s typical   │  NAT/bridged attachment, graphics)
  │        on Apple Silicon │  vm.Start(); guest kernel + launchd +
  │◀────────────────────────│  guest agent come up
  └────────────┬────────────┘
               ▼  event: VMDialing
  ┌─────────────────────────┐
  │ AGENT HANDSHAKE         │  host accepts vsock conn (45s cap)
  │                         │  → Handshake{token, nonce} → OK
  │        ≤10s deadline    │  → Ready check (≤2m budget)
  └────────────┬────────────┘
               ▼
  ┌─────────────────────────┐
  │ GUEST SETUP             │  Materialize volumes into place
  │                         │  NetInfo → PodIP   event: Started
  │                         │  postStart hook → sidecars → Running
  └────────────┬────────────┘
               ▼
        probes begin immediately (initialDelay honored),
        readiness flips endpoints in, liveness armed
```

Two details in that waterfall carry disproportionate weight.

The first is the **MAC address lease**. Each pod UID gets a stable, locally-
administered MAC persisted across restarts (`mac.txt`, written via
temp-file-rename so a crash mid-write can't corrupt it). DHCP leases stay
sticky, the ARP-cache fallback stays meaningful, and a liveness-triggered VM
restart doesn't look like new hardware to the network.

The second is the **dial discipline**. Early versions tried transports
sequentially — vsock accept to completion, then connect, then TCP — which
meant a wedged vsock consumed the whole boot budget before the fallback ever
got its turn, silently, while the user stared at `Pending`. Today the dial is
capped at 45 seconds regardless of caller context, progress is narrated as
events (`Created → Starting → VMDialing → Started`) so `kubectl describe`
reads like a story instead of a shrug, and the host is strictly an
*acceptor*: on timeout the listener closes and any late guest connection is
refused, because a control channel that connects to whoever knocks first is
not a control channel.

---

## 7. Hybrid pods: one foot in each world <a name="7-hybrid"></a>

The unique capability of this node is that one pod can straddle two
operating systems:

```
spec:
  containers:
  - name: builder            ← macOS VM (containers[0])
      image: ci/sonoma-xcode16:25.1
      resources: {requests: {cpu: "6", memory: 24Gi}}
  - name: artifact-proxy     ← Linux sidecar on the HOST (containers[1])
      image: nginx:alpine
      ports: [{containerPort: 8080, hostPort: 8080}]
  initContainers:
  - name: fetch-manifest     ← host-side, sequential, must exit 0
      image: busybox
```

Container zero *is* the VM. Containers one-through-N are ordinary Linux
containers on the host's Docker daemon, wired with the mounts and resource
limits from their spec, sharing nothing with the guest but the pod's identity.
Init containers run sequentially on the host before the VM exists — with an
explicit guard rejecting VM images in init position, because "init" that
boots an operating system is a category error, not a slow start.

The classic CI topology this enables — a macOS build environment with a thin
Linux logging or caching sidecar — previously required two nodes and a
service boundary. Here it is one pod, one lifetime, one delete call.

Volumes follow the same split-brain logic. The host materializes everything
it can materialize — emptyDir, hostPath (allowlisted!), ConfigMaps, Secrets,
projected tokens — into a per-UID tree, shares those directories over
virtio-fs, and the guest agent places them at the mount paths:

```
HOST                                        GUEST
cache/pods/<uid>/
├── volumes/
│   ├── build-cache/   ── virtio-fs ──▶ /Volumes/My Shared Files/build-cache
│   ├── tls-secret/                        │
│   │     ca.crt      0644                 ├─ link mode:  symlink → mount path
│   │     tls.key      0600  ─┐           └─ copy mode:  byte-for-byte copy
│   └── sa-token/              │                (secrets ALWAYS copy)
│         token         ◀─────┘
├── control/  (read-only share)
│     agent-token  0600   ← the handshake secret lives here, briefly
└── overlay/  {disk.img, aux.img}   ← the VM's actual root, CoW-cloned
```

Mode selection is a policy statement dressed as an implementation detail.
Regular volumes default to *link* — a symlink, so the share stays the source
of truth and updates flow through. Anything carrying credentials defaults to
*copy*: the secret's bytes are duplicated into the guest and the automount
copy becomes inert scaffolding rather than a permanent window onto the
secret. Teardown mirrors creation in reverse — preStop hook, agent shutdown,
VM stop confirmed, *then* `RemoveAll` — because deleting files underneath a
still-running virtio-fs client is how you turn a clean shutdown into silent
data loss.

Projected service-account tokens are real ones: minted via the TokenRequest
API with audience and expiry, not a static string. Bound-token semantics
survive the trip into the guest.

---

## 8. Networking without a CNI <a name="8-networking"></a>

There is no CNI plugin for Virtualization.framework. There is, however, a
switch in the VM config, and the node refuses to pretend the two settings
are interchangeable:

```
      NAT MODE (default)                      BRIDGED MODE (opt-in)
      laptop / CI                             production Services

  pod VM ──┐                                  pod VM ──┐
  pod VM ──┤── vmnet NAT ──▶ Mac's uplink     pod VM ──┤── bridge ──▶ LAN
           │                                           │            │
  PodIP: host-local ONLY                    PodIP: routable L2 ◀─┘
  NOT a ClusterIP backend                   real ClusterIP backend
           │                                           │
  tainted: darwin.node/nat-only             requires com.apple.vm.networking
  unless --allow-nat-workloads              (Apple-approved entitlement)
```

The taint is the load-bearing part. NAT mode is perfectly good for `kubectl
exec`, port-forwards, and CI drivers — and perfectly broken for Services,
because kube-proxy on some *other* node cannot route to an address that only
exists inside this Mac's NAT. Rather than letting users discover that with a
weekend of debugging, NAT-only nodes wear a `NoSchedule` taint that only an
explicit flag removes. The node knows things the scheduler doesn't; it is
obliged to say so.

`PodIP` comes from the horse's mouth — the guest agent reports its interfaces
after DHCP — because the alternatives (ARP-table scraping, packet sniffing)
are races wearing a trench coat. hostPort is a small userspace TCP proxy that
binds on the host and splices bytes toward `PodIP:containerPort`,
re-resolving destinations when a restarted VM changes address. UDP hostPorts
fail loudly rather than silently doing nothing. Port-forward remains the one
door marked "not yet": the agent protocol could carry it trivially, and the
ADR saying so is written; shipping half a feature is worse than documenting
an absent one.

---

## 9. Trust boundaries <a name="9-trust"></a>

Security review found its clearest expression as a diagram, so here is the
whole model. Arrows mean "data or commands cross here":

```
 ┌──────────────────────────────────────────────────────────────────────┐
 │ CLUSTER                                                              │
 │   kube-apiserver ◀── mTLS, client-cert verification required ──┐     │
 └────────────────────────────────────────────────────────────────┼─────┘
                                                                  │
 ┌──────────────────────── HOST (the Mac) ────────────────────────┼─────┐
 │                                                                ▼     │
 │  darwin-node: privileged, Virtualization-entitled binary       │     │
 │  • HTTP server refuses to start without certs (cluster mode)   │     │
 │  • CacheDir 0700 — bootable OS images are code                 │     │
 │  • hostPath: deny-by-default allowlist; "/" never permitted    │     │
 │  • secrets at rest: 0700 dirs, 0600 files, swept after 24h     │     │
 │                                                                │     │
 │  ┌── vsock (device bus — no IP, no neighbors) ─────────────┐   │     │
 └──┼──────────────────────────────────────────────────────────┼───┼─────┘
    │                                                          │   
 ┌──▼────────────── GUEST (a whole other OS) ────────────────┐ │    
 │  guest agent: root IN THE GUEST, which is not the host    │ │    
 │  • handshake: per-pod token + nonce, else total silence   │ │    
 │  • deadlines: 10s handshake, 30s idle, 60s probe clamp    │ │    
 │  • ceilings: 8 conns · 4 execs · 8 inflight · capped logs │ │    
 │  • exec argv crosses as argv; no shell interpretation     │ │    
 └───────────────────────────────────────────────────────────┘ │    
                                                               │    
 ┌── Docker socket (host) ─────────────────────────────────────▼────┐
 │  Linux sidecars: spec-derived mounts + limits, named, reaped     │
 └──────────────────────────────────────────────────────────────────┘
```

Three rules govern every line of that picture.

**Fail closed is a verb, not a slogan.** Empty token rejects all handshakes.
Missing certs refuse node startup. Missing provenance rejects image boots.
Unrecognized volume types fail admission instead of failing forty seconds
later somewhere confusing. Every one of those was, at some point, a bug that
failed open — which is why the rule is phrased as a search-and-destroy
mission rather than a design principle.

**The escape hatches are loud.** Dev-only insecure token acceptance, TCP
fallback, and SSH each require an explicit flag, and SSH additionally demands
a pinned host key and refuses passwords outright. A fallback that activates
silently isn't a fallback; it's a second, unaudited product.

**Anything that can write the cache can rewrite reality** — hence `0700`, and
hence provenance anchored at pull time rather than reconstructed from local
metadata. The remaining gap (signature verification, cosign-style) is
documented deferral, not an oversight.

---

## 10. Telling the truth to the scheduler <a name="10-truth"></a>

Virtual Kubelet's implicit contract is brutal: *the provider is the source of
truth for pod status.* Everything downstream — the scheduler's assumptions,
metrics-server, HPA, the human running `kubectl top` — inherits whatever the
provider asserts. Assert fiction and you haven't merely misreported; you've
poisoned autoscaling signals for the whole cluster.

So the reporting layer is built on one negative rule: **never fabricate a
number you did not measure.**

- Node CPU/memory usage comes from host measurement, pod CPU/memory from the
  guest agent's own sampler over vsock. If the agent doesn't answer, that
  pod's stats are *omitted* — a gap metrics-server tolerates — rather than
  replaced with capacity-as-usage, which it does not.
- Per-container stats exist because `kubectl top pods` renders containers,
  not just pods; a summary missing them is technically valid and practically
  broken.
- Conditions and taints are recomputed from the live slot table and host
  probes on a heartbeat; `vm-full` appears when the second slot fills, not
  when the node rebooted.
- Events narrate the lifecycle in the vocabulary operators already know —
  `Pulling/Pulled`, `Starting`, `VMDialing`, `Started`, `Killing`,
  `ProbeFailed` — so `kubectl describe pod` tells the same story the engine
  is living.

The embarrassing part of this section is how late it arrived: the first
stats implementation reported host *capacity* as usage, which made every
dashboard show a permanently saturated node and taught us the rule above the
expensive way. Honesty requirements that seem obvious in retrospect usually
have a scar under them.

---

## 11. What we got wrong <a name="11-wrong"></a>

A architecture post that only lists victories is a brochure. The ledger,
in fairness:

**The engine is fire-and-forget where it should be a reconciler.** Pod state
is written once during `start()` and amended by probes; the VM's actual power
state is read and then discarded in the status path. A guest that panics
leaves a pod reporting `Running` until a probe notices something is wrong —
and "noticing" never escalates to the restart policy the spec asked for.
Node-level conditions got their heartbeat; pod-level truth is still waiting
for the same treatment. This is the largest open item, and everything
lifecycle-shaped composes on top of it.

**Identity is keyed by namespace/name while capacity is keyed by UID.** The
delete-and-recreate race — Deployment rolls, old and new pod briefly share a
name — can wedge a record that silently ignores its replacement. The fix
(re-key records on UID, treat same-name-different-UID as replace) is known,
small, and still undone.

**Exec is half-streamed.** stdout/stderr now flow through live, but there is
no stdin path and no pty, so `kubectl exec -it` — the first thing every human
types — degrades to command-mode. The protocol has room reserved for input
frames; the plumbing is unfinished.

**`logs -f` returns a snapshot.** Follow was designed, deferred, documented,
and users still hit it weekly. Log persistence (for `previous`-container
logs) rides on the same unbuilt substrate.

**SIGINT kills every VM like a power cut.** The delete path honors grace
periods scrupulously; the process-exit path does not. A graceful drain loop
between signal handler and engine is specified and pending.

None of these are mysteries. They are queued, evidenced, and tracked in the
open — which is what an honest backlog looks like, and also what the next
section is about.

---

## 12. The philosophy <a name="12-philosophy"></a>

Strip away the Mac-specific machinery and four rules remain, none of them
novel, all of them expensive to learn:

**Design around the constraint you cannot remove.** The two-VM quota shaped
admission, images, boot, and status. Fighting it would have produced a
system that lies at every layer; accepting it produced one that lies at none.

**Make the wrong thing impossible before making the right thing easy.**
Fail-closed admission, deny-by-default hostPath, mandatory-TLS, accept-only
control channels — every one removes a failure mode rather than handling it.

**Never report a number you didn't measure.** A virtual-kubelet's cardinal
sin is feeding the scheduler plausible fiction. Gaps are recoverable; lies
compound.

**Boring components, sharp glue.** ORAS, Virtualization.framework, APFS
clonefile, virtio-fs, launchd, plain JSON frames — nothing exotic in the
inventory. The system's character lives entirely in how the pieces are
constrained, ordered, and told the truth about each other.

The Mac was never supposed to be a cloud. It has no namespaces, no cgroups,
no container ecosystem, a licensing regime instead of a resource manager,
and a hypervisor that counts to two. But it has the toolchain that a large
fraction of the world's mobile software depends on, and it has hardware that
makes Linux runners look like they're wading through syrup.

Meeting Kubernetes halfway meant building the missing half honestly: a node
that admits what it is — two computers, occasionally three processes — and
lets the cluster trust it anyway.

That's the whole trick. The rest is plumbing, and the plumbing is open.
