//go:build !race

package vzcontract

// fdSlack is zero unless the race detector is linked. A couple of
// descriptors stay open under -race; growth past that is a leaked socket.
const fdSlack = 0
