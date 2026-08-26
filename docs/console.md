# Break-glass Console

The serial console is the rescue hatch for the failure mode SSH-era
nodes cannot handle: the in-guest agent is down, SSH is disabled, and
you still need single-user mode. It attaches a VM serial port that is
independent of `darwin-guest-agent`, of sshd, and of the network stack.

## Enabling

```
flag   --serial-console        attach serial ports (off by default)
env    DARWIN_NODE_SERIAL_CONSOLE=true
```

With the flag set, every VM created by the vz runtime gets one
`VZFileHandleSerialPortAttachment` backed by two os.Pipe pairs: host
writes flow into the guest UART, guest UART output flows back to the
host. The handles live for the life of the VM; there is no reconnection
and no buffer replay. Bytes written before anything reads them are lost,
which matches real serial hardware.

## Attaching

The node serves each running pod's console on a host-local unix socket:

```
$(DarwinTempDir)/darwin-console-<sha256(namespace/name)[:8]>.sock
```

The path is deterministic per namespace and pod name so `darwin-node
console` resolves it without querying the node. Sockets live in the
system temp directory because macOS limits unix socket paths to 104
bytes and node cache directories nest too deeply.

```bash
darwin-node console --namespace default --name macos
```

The CLI puts the local terminal into raw mode (`x/term.MakeRaw`) and
copies both directions until the remote side closes. In raw mode ^C and
other control bytes reach the guest line discipline untouched, which is
what makes single-user debugging work. Detach by closing the terminal;
there is deliberately no server-side escape sequence, because the point
of this channel is that nothing intercepts it.

## Trust model

- The socket inherits the node process's file permissions; only a local
  administrator can reach it.
- The console exposes the guest exactly as a monitor cable would. It is
  more powerful than the agent protocol and exists precisely because it
  bypasses authentication inside the guest. Enable `--serial-console`
  on machines whose operators you already trust with root on the host.
- Concurrent attachments share the byte stream; two operators typing at
  once interleave, as on a shared physical console.
