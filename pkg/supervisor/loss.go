package supervisor

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
)

// watchLoss reports supervisor death from EVFILT_PROC / pidfd and from
// the control or guest connection closing. lost runs once across both
// signals. ESRCH while registering the pid means the process is already
// dead, and lost runs before watchLoss returns.
//
// stop wakes both waits and joins them. It does not close pid, ctrlFD,
// or guestFD. Calling stop twice is safe.
func watchLoss(pid, ctrlFD, guestFD int, lost func()) (stop func(), err error) {
	if lost == nil {
		lost = func() {}
	}
	var once sync.Once
	fire := func() { once.Do(lost) }

	var proc *procHandle
	if pid > 0 {
		h, herr := openProcWatch(pid)
		if herr != nil {
			if errors.Is(herr, unix.ESRCH) || errors.Is(herr, ErrProcGone) {
				fire()
			} else {
				return nil, herr
			}
		} else {
			proc = h
		}
	}

	fdR, fdW, err := cloexecPipe()
	if err != nil {
		if proc != nil {
			_ = proc.close()
		}
		return nil, err
	}
	procR, procW := -1, -1
	if proc != nil {
		procR, procW, err = cloexecPipe()
		if err != nil {
			_ = unix.Close(fdR)
			_ = unix.Close(fdW)
			_ = proc.close()
			return nil, err
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if werr := watchFDs(fdR, ctrlFD, guestFD); werr != nil {
			fire()
		}
	}()
	if proc != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exited, werr := proc.wait(procR)
			// A watch that fails is treated as loss. A clean stop
			// returns false and a nil error, which is not a loss.
			if werr != nil || exited {
				fire()
			}
		}()
	}

	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() {
			poke(fdW)
			poke(procW)
			wg.Wait()
			_ = unix.Close(fdR)
			_ = unix.Close(fdW)
			if procR >= 0 {
				_ = unix.Close(procR)
				_ = unix.Close(procW)
			}
		})
	}
	return stop, nil
}

// WaitExit blocks until pid has exited. A pid that is already gone is
// success. The wait is pidfd on Linux and EVFILT_PROC NOTE_EXIT on Darwin.
func WaitExit(pid int) error {
	if pid <= 0 {
		return ErrProcGone
	}
	h, err := openProcWatch(pid)
	if errors.Is(err, unix.ESRCH) || errors.Is(err, ErrProcGone) {
		return nil
	}
	if err != nil {
		return err
	}
	exited, err := h.wait(-1)
	if err != nil {
		return err
	}
	if !exited {
		return fmt.Errorf("process %d watch ended", pid)
	}
	return nil
}

func cloexecPipe() (int, int, error) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		return -1, -1, err
	}
	unix.CloseOnExec(p[0])
	unix.CloseOnExec(p[1])
	if err := unix.SetNonblock(p[1], true); err != nil {
		_ = unix.Close(p[0])
		_ = unix.Close(p[1])
		return -1, -1, err
	}
	return p[0], p[1], nil
}

func poke(fd int) {
	if fd < 0 {
		return
	}
	_, err := unix.Write(fd, []byte{1})
	if err == nil || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return
	}
}
