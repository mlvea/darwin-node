package engine

// PendingTimersForTest is the number of virtual-clock waits that have not
// fired or been stopped. The conformance suite advances time only after a
// backoff timer exists, so Advance cannot land before NewTimer arms it.
func (c *VirtualClock) PendingTimersForTest() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.waiters {
		if t != nil && !t.stopped {
			n++
		}
	}
	return n
}
