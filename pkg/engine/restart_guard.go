package engine

import "sync"

// restartingPods marks a Stop that restartVM owns. The probe watch's
// first pass can still be queued when that Stop runs. Without this, the
// watch reads VMStopped and fails the pod out from under the restart.
var restartingPods sync.Map

func markRestarting(rec *podRecord) {
	if rec == nil {
		return
	}
	restartingPods.Store(rec, struct{}{})
}

func clearRestarting(rec *podRecord) {
	if rec == nil {
		return
	}
	restartingPods.Delete(rec)
}

func isRestarting(rec *podRecord) bool {
	if rec == nil {
		return false
	}
	_, ok := restartingPods.Load(rec)
	return ok
}
