package nodecert

import "golang.org/x/sys/unix"

func unixOpenDir(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

func unixClose(fd int) { _ = unix.Close(fd) }

func fchmod(fd int, mode uint32) error { return unix.Fchmod(fd, mode) }
