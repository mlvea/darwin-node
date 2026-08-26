package event

import (
	"context"
	"log/slog"
)

// Recorder is a subset of kubelet events. Implementations must not panic.
type Recorder interface {
	Normal(ctx context.Context, reason, message string)
	Warn(ctx context.Context, reason, message string)
}

// Nop is a no-op recorder.
type Nop struct{}

func (Nop) Normal(context.Context, string, string) {}
func (Nop) Warn(context.Context, string, string)   {}

// Slog records to slog.
type Slog struct{ L *slog.Logger }

func (s Slog) Normal(_ context.Context, reason, message string) {
	log := slog.Default()
	if s.L != nil {
		log = s.L
	}
	log.Info("event", "reason", reason, "message", message)
}

func (s Slog) Warn(_ context.Context, reason, message string) {
	log := slog.Default()
	if s.L != nil {
		log = s.L
	}
	log.Warn("event", "reason", reason, "message", message)
}

// Reasons.
const (
	ReasonPulling             = "Pulling"
	ReasonPulled              = "Pulled"
	ReasonCreated             = "Created"
	ReasonStarting            = "Starting"
	ReasonVMDialing           = "VMDialing"
	ReasonStarted             = "Started"
	ReasonFailed              = "Failed"
	ReasonFailedCreate        = "FailedCreate"
	ReasonFailedStart         = "FailedStart"
	ReasonKilling             = "Killing"
	ReasonVMCapacityExhausted = "VMCapacityExhausted"
	ReasonGuestAgent          = "GuestAgent"
	ReasonProbeFailed         = "ProbeFailed"
	ReasonPreStopFailed       = "FailedPreStopHook"
	ReasonPostStartFailed     = "FailedPostStartHook"
	ReasonVMRestarted         = "VMRestarted"
	ReasonPodIPChanged        = "PodIPChanged"
	ReasonInitStarted         = "InitContainerStarted"
	ReasonInitExited          = "InitContainerExited"
	ReasonWarmBooted          = "WarmVMBooted"
	ReasonWarmAdopted         = "WarmVMAdopted"
	ReasonWarmEvicted         = "WarmVMEvicted"
	ReasonCacheRestored       = "CacheRestored"
	ReasonCacheSaved          = "CacheSnapshotSaved"
	ReasonDraining            = "NodeDraining"
)
