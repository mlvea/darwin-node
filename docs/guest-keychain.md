# Guest ephemeral keychain

Every job that boots from one golden image otherwise shares that image's
login keychain and any credentials baked into it. Before Workload, the
guest agent installs a fresh keychain for this pod and takes the baked
user keychains off the user search list. A failure fails the pod. The
workload does not start.

The password is generated in the guest. It is not a field on the request
or the response, and it is not written into the pod status.

## Wire

Protocol version stays 1. The method is `EphemeralKeychain` and the
capability is `ephemeral-keychain`. An older agent omits the capability
and answers `unknown_method`. JSON names are snake_case. There is no
password field and no session-token field.

Request, host to guest:

| Field | Meaning |
| --- | --- |
| `pod_uid` | Pod UID. Non-empty, at most 1024 bytes, no control characters, and a single path component. |

Response:

| Field | Meaning |
| --- | --- |
| `ok` | True only when every guest step succeeded. |
| `keychain_path` | `<workload user home>/Library/Keychains/darwin-node-job-<pod uid>.keychain-db`. |
| `default_keychain` | The user default keychain. It is `keychain_path`. |
| `user_search_list` | The user-domain search list. It is that one path. |
| `login_hidden` | True when `login.keychain-db` and `login.keychain` are no longer at their well-known paths. |
| `system_keychain` | The System keychain path. It is still present. |

The host checks that shape before it calls Workload. A response that
leaves the login keychain on the user search list, or that reports the
System keychain as the job file, fails the pod.

## Guest steps

Any error stops the sequence. The guest does not return `ok`.

1. Validate the pod UID. Nothing touches the OS before this returns.
2. Draw a fresh password from `crypto/rand`. It is not logged.
3. Resolve the workload user's home. The guest agent and the workload run as the same user, so this is that user's home (`os.UserHomeDir`; root when launchd starts the system daemon).
4. Create `Library/Keychains/darwin-node-job-<pod uid>.keychain-db` under that home.
5. Unlock it and disable auto-lock for the job.
6. Make it the user default keychain.
7. Set the user search list to that one path. The System keychain is not added and is not deleted.
8. Move `login.keychain-db` and `login.keychain`, when they exist, to the same name with `.darwin-node-hidden` appended. A missing login keychain is success. A directory at that path fails the step.

On workload exit and on Shutdown, the guest deletes the job keychain file.
It does not delete the System keychain or the hidden login keychain.
Deleting a missing job keychain is success, so exit and Shutdown can both
run.

The OS steps sit behind `KeychainPlatform`. Linux tests, SimVZ, and the
fake runtime use an in-memory platform that starts from the image login
keychain and the System keychain. `cmd/guest-agent` on darwin installs the
real platform. `cmd/guest-agent` on any other GOOS installs the in-memory
one.

`DARWIN_NODE_GUEST_SOFTWARE_PLATFORM=1` (or `true`) forces the in-memory
platform even on darwin. SimVZ sets it when it execs `darwin-guest-agent`.
The launchd job inside a VM image does not set it. Default off.

On darwin the password is passed to Security.framework (`SecKeychainCreate`,
`SecKeychainUnlock`, `SecKeychainSetSettings`). It is not an argument of
the `security` CLI. The CLI is used for `default-keychain -d user` and
`list-keychains -d user`, which do not take the password.

Inside a VM the agent has to be able to create a keychain for the user it
runs as. Those calls fail closed without that.

## Host checks

On a cold start and on a warm-pool handoff the host calls
`EphemeralKeychain` after Adopt and volume placement, and before
Workload. A VM restart reseeds, installs the keychain, and then starts
the workload. Copy-mode volumes are placed again after that. A redial
of the same boot (`recoverAgent`) does not install another keychain.
An equal-major supervisor re-adopt whose handshake says the workload
has not started sends this call once, on the way to Workload.

On failure the host closes the agent, stops the VM, and marks the pod
Failed. Workload is not called. This is a permanent start failure. Retrying
the same image will not produce a keychain the guest cannot install.

## Older guests

Missing capability `ephemeral-keychain`, or an `unknown_method` response,
fails the pod. `DARWIN_NODE_ALLOW_LEGACY_GUEST=1` or `true` skips only that
case and logs a warning. Default off. A failed create, unlock, default,
search-list, or hide still fails the pod. The variable is the same one
Adopt reads. It is not a field on the node config.

## Hardware-pending

These need a Mac Mini and two pods of one image, as the user the guest
agent runs as. Linux CI covers the protocol, the in-memory directory, the
ordering, and the fail-closed host with SimVZ. The calls below are not
executed in CI.

- `security list-keychains -d user` for the workload user prints only `~/Library/Keychains/darwin-node-job-<pod uid>.keychain-db`.
- `security default-keychain -d user` prints that same file.
- `security show-keychain-info` on that file reports no lock timeout.
- `login.keychain-db` is not on the user search list. The file was renamed with a `.darwin-node-hidden` suffix and was not deleted. `/Library/Keychains/System.keychain` is still present.
- After the pod ends, `darwin-node-job-<pod uid>.keychain-db` is gone.
- The next pod's `security list-keychains -d user` and `security default-keychain -d user` name a different `darwin-node-job-<other uid>.keychain-db`.
- The keychain password does not appear in the guest agent log or the pod status.
