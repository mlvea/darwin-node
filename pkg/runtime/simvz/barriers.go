package simvz

// Barrier names are the async points a test can arm. pkg/vzcontract
// repeats the same strings; the contract test checks the two lists match.
const (
	BarrierBeforeGuestDidStop      = "before_guest_did_stop"
	BarrierBeforeKVOStopped        = "before_kvo_stopped"
	BarrierBeforeStopCompletion    = "before_stop_completion"
	BarrierBeforeKVOStarting       = "before_kvo_starting"
	BarrierBeforeKVORunning        = "before_kvo_running"
	BarrierBeforeStartCompletion   = "before_start_completion"
	BarrierBeforeKVOError          = "before_kvo_error"
	BarrierBeforeConnectCompletion = "before_connect_completion"
	BarrierBeforeSaveCompletion    = "before_save_completion"
	BarrierBeforeRestoreCompletion = "before_restore_completion"
	BarrierBeforeKVOStopping       = "before_kvo_stopping"
	BarrierBeforeKVOPausing        = "before_kvo_pausing"
	BarrierBeforeKVOSaving         = "before_kvo_saving"
	BarrierBeforeKVOResuming       = "before_kvo_resuming"
	BarrierBeforeKVORestoring      = "before_kvo_restoring"
)

func allBarriers() []string {
	return []string{
		BarrierBeforeGuestDidStop,
		BarrierBeforeKVOStopped,
		BarrierBeforeStopCompletion,
		BarrierBeforeKVOStarting,
		BarrierBeforeKVORunning,
		BarrierBeforeStartCompletion,
		BarrierBeforeKVOError,
		BarrierBeforeConnectCompletion,
		BarrierBeforeSaveCompletion,
		BarrierBeforeRestoreCompletion,
		BarrierBeforeKVOStopping,
		BarrierBeforeKVOPausing,
		BarrierBeforeKVOSaving,
		BarrierBeforeKVOResuming,
		BarrierBeforeKVORestoring,
	}
}
