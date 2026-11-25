# VZ transition traces

These files are a **derived** corpus. They were written from the Day-1
contract in `pkg/vzcontract`, not recorded on a Mac. Every `meta` line says
`source` is `derived` and the note contains `not hardware-recorded`.

Do not treat a green Linux replay as evidence that Virtualization.framework
emits these orderings. That check is hardware-pending. See
`docs/vzcontract.md` for the Mac Mini run and for how to promote a trace.

## Schema

One JSON object per line:

```json
{"seq":4,"op":"stop","pre_state":"Running","events":[{"kind":"state_change","state":"Stopping"},{"kind":"delegate_guest_did_stop"},{"kind":"state_change","state":"Stopped"},{"kind":"completion_handler","err":null}]}
```

`seq` starts at 1 and increases by one. `op` is `meta`, `create`, `start`,
`stop`, `request_stop`, `pause`, `resume`, `save`, `restore`, `connect`,
`register`, or `crash`. `meta` carries `order`, `hold_agent`, `source`, and
`note`. `events` is the list the runtime must produce for that op, and only
that op. A missing `events` field does not assert a list.

Completion `err` is either `null` or an object with `domain`/`code`/`name`
(a VZErrorDomain NSError) or `errno` (`ECONNREFUSED`, `EPIPE`, `ECONNRESET`,
`ENOBUFS`, `EAGAIN`). Replay rejects a completion whose code is not in the
file's envelope.

`vm` names a machine. Several creates share one runtime, which is how the
third-VM trace holds two slots.

## Files

| File | What it pins |
| --- | --- |
| `stop_kvo_before_completion.jsonl` | guestDidStop, KVO Stopped, completion |
| `stop_completion_before_kvo.jsonl` | guestDidStop, completion, KVO Stopped |
| `request_stop_kvo_before_completion.jsonl` | RequestStop uses Stop's notifications |
| `third_vm_limit.jsonl` | Create is free; the third Start fails with code 6 and ends in Error |
| `save_from_running.jsonl` | Save from Running is code 3 |
| `connect_refused.jsonl` | Connect fails fast with ECONNREFUSED until the agent registers |
| `save_restore.jsonl` | Save stays Paused; Restore from Stopped ends Paused |
