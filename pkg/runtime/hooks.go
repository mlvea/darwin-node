package runtime

import (
	"context"
	"os"
)

// PauseSaver is an optional machine capability. The per-VM supervisor
// calls it for PauseAndSaveVM. A machine that cannot save omits the
// method; the supervisor then reports that the RPC is unsupported.
type PauseSaver interface {
	PauseAndSave(ctx context.Context, path string) error
}

// ChannelSource is an optional machine capability. OpenChannels returns
// two caller-owned files and does not read either of them:
//
//   - netFD is a dup of the supervisor end of the SOCK_DGRAM socketpair
//     attached as the guest NIC (VZFileHandleNetworkDeviceAttachment).
//   - agentFD is a connected stream to the guest agent. The supervisor
//     dials it and hands the descriptor to the node agent. The guest
//     protocol is not parsed on the supervisor side of that handoff.
//
// The caller closes both files.
type ChannelSource interface {
	OpenChannels(ctx context.Context) (netFD, agentFD *os.File, err error)
}

// ChannelCloser releases descriptors OpenChannels kept for the life of
// the VM. Stop may also close them. CloseChannels is idempotent.
type ChannelCloser interface {
	CloseChannels() error
}
