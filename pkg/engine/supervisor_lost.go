package engine

import (
	"strings"

	"github.com/mlvea/darwin-node/pkg/types"
)

// supervisorLostReason is the pod reason and message when the per-VM
// supervisor disappears. The adapter publishes the same text on the
// machine. Restart policy must not boot a replacement.
const supervisorLostReason = "supervisor lost"

// supervisorLost reports the adapter's lost-supervisor status. A VM that
// failed for any other reason keeps the generic VMFailed text.
func supervisorLost(st types.VMStatus) bool {
	if st.State != types.VMFailed && st.State != types.VMStopped {
		return false
	}
	return strings.TrimSpace(st.Message) == supervisorLostReason
}
