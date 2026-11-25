package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
)

func (s *Server) dispatch(ctx context.Context, req message) (message, []*osFile, bool, error) {
	switch req.Op {
	case opStart:
		msg, err := s.handleStart(ctx, req)
		return msg, nil, false, err
	case opInspect:
		return s.handleInspect(), nil, false, nil
	case opPause:
		msg, err := s.handlePause(ctx, req)
		return msg, nil, false, err
	case opStop:
		msg, done, err := s.handleStop(ctx, req)
		return msg, nil, done, err
	case opAttach:
		msg, fds, err := s.handleAttach(ctx)
		return msg, fds, false, err
	case opHeartbeat:
		s.touch()
		msg := s.inspect()
		msg.OK = true
		return msg, nil, false, nil
	default:
		return errMsg("", "unknown supervisor op "+req.Op), nil, false, nil
	}
}

// osFile is an alias so the dispatch signature stays readable next to os.File.
type osFile = os.File

func (s *Server) handleStart(ctx context.Context, req message) (message, error) {
	if req.Spec == nil {
		return errMsg("", "start_vm requires a spec"), nil
	}
	if req.BootID == "" || req.HMACToken == "" {
		return errMsg("", "start_vm requires boot_id and hmac_token"), nil
	}
	s.vmOp.Lock()
	defer s.vmOp.Unlock()

	s.mu.Lock()
	existing := s.vm
	same := existing != nil && s.bootID == req.BootID
	s.mu.Unlock()
	if same {
		msg := s.inspect()
		msg.OK = true
		return msg, nil
	}
	if existing != nil {
		return errMsg(codeBusy, "slot already has a vm"), nil
	}

	m, err := s.RT.Create(ctx, *req.Spec)
	if err != nil {
		return message{}, err
	}
	if err := m.Start(ctx); err != nil {
		_ = m.Stop(context.Background(), 0)
		return message{}, err
	}
	digest := req.ImageDigest
	if digest == "" {
		digest = req.Spec.ImageRef
	}
	s.mu.Lock()
	if s.vm != nil {
		s.mu.Unlock()
		_ = m.Stop(context.Background(), 0)
		return errMsg(codeBusy, "slot already has a vm"), nil
	}
	s.vm = m
	s.bootID = req.BootID
	s.token = req.HMACToken
	s.digest = digest
	s.ns = req.Spec.ID.Namespace
	s.podName = req.Spec.ID.Name
	s.podUID = req.Spec.ID.UID
	s.paused = false
	s.mu.Unlock()
	slog.Info("supervisor vm started", "slot", s.Slot, "boot_id", req.BootID, "pod", req.Spec.ID.String())
	msg := s.inspect()
	msg.OK = true
	return msg, nil
}

func (s *Server) handleInspect() message {
	s.vmOp.Lock()
	defer s.vmOp.Unlock()
	msg := s.inspect()
	msg.OK = true
	return msg
}

func (s *Server) handlePause(ctx context.Context, req message) (message, error) {
	if req.SavePath == "" {
		return errMsg("", "pause_and_save_vm requires save_path"), nil
	}
	s.vmOp.Lock()
	defer s.vmOp.Unlock()
	s.mu.Lock()
	m := s.vm
	s.mu.Unlock()
	if m == nil {
		return errMsg(codeNoVM, "no vm"), nil
	}
	saver, ok := m.(runtime.PauseSaver)
	if !ok {
		return errMsg(codePause, "pause and save is not supported by this runtime"), nil
	}
	if err := saver.PauseAndSave(ctx, req.SavePath); err != nil {
		return message{}, err
	}
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
	msg := s.inspect()
	msg.OK = true
	msg.Paused = true
	return msg, nil
}

func (s *Server) handleStop(ctx context.Context, req message) (message, bool, error) {
	grace := time.Duration(req.GraceMS) * time.Millisecond
	s.vmOp.Lock()
	stopped, err := s.stopVM(ctx, grace)
	s.vmOp.Unlock()
	if err != nil {
		return message{}, false, err
	}
	msg := s.inspect()
	msg.OK = true
	msg.State = string(types.VMStopped)
	// Exit only when a VM was actually stopped. A stop with no VM leaves
	// the process up so the agent can retry StartVM on the same socket.
	return msg, stopped, nil
}

func (s *Server) handleAttach(ctx context.Context) (message, []*os.File, error) {
	s.vmOp.Lock()
	defer s.vmOp.Unlock()
	s.mu.Lock()
	m := s.vm
	s.mu.Unlock()
	if m == nil {
		return errMsg(codeNoVM, "no vm"), nil, nil
	}
	src, ok := m.(runtime.ChannelSource)
	if !ok {
		return errMsg("", "runtime cannot hand off vm channels"), nil, nil
	}
	netFD, agentFD, err := src.OpenChannels(ctx)
	if err != nil {
		return message{}, nil, err
	}
	msg := s.inspect()
	msg.OK = true
	msg.FDCount = 2
	return msg, []*os.File{netFD, agentFD}, nil
}

func (s *Server) stopVM(ctx context.Context, grace time.Duration) (bool, error) {
	s.mu.Lock()
	m := s.vm
	s.vm = nil
	s.paused = false
	s.bootID = ""
	s.token = ""
	s.digest = ""
	s.ns = ""
	s.podName = ""
	s.podUID = ""
	s.mu.Unlock()
	if m == nil {
		return false, nil
	}
	if c, ok := m.(runtime.ChannelCloser); ok {
		_ = c.CloseChannels()
	}
	err := m.Stop(ctx, grace)
	slog.Info("supervisor vm stopped", "slot", s.Slot, "err", errString(err))
	return true, err
}

func (s *Server) inspect() message {
	s.mu.Lock()
	m := s.vm
	msg := message{
		PID:           os.Getpid(),
		StartTimeNS:   s.started.UnixNano(),
		BinaryVersion: s.version,
		ProtocolMajor: s.major,
		ProtocolMinor: s.minor,
		Slot:          s.Slot,
		BootID:        s.bootID,
		HMACToken:     s.token,
		ImageDigest:   s.digest,
		Namespace:     s.ns,
		PodName:       s.podName,
		PodUID:        s.podUID,
		Paused:        s.paused,
		State:         string(types.VMPending),
	}
	s.mu.Unlock()
	if m == nil {
		return msg
	}
	st := m.Status()
	msg.State = string(st.State)
	msg.IP = st.IP
	msg.MAC = st.MAC
	msg.AgentOK = st.AgentOK
	msg.Message = st.Message
	msg.ExitCode = st.ExitCode
	if st.Err != nil && msg.Message == "" {
		msg.Message = st.Err.Error()
	}
	return msg
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
