//go:build linux || darwin || freebsd || openbsd || netbsd

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// readKey reads one key press without waiting for Enter. Ctrl+C and Ctrl+D
// come back as bytes (3 and 4) so the caller can stop cleanly.
func readKey(in *os.File) (byte, error) {
	fd := int(in.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return readKeyLine(in)
	}
	raw := *old
	raw.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return readKeyLine(in)
	}
	defer unix.IoctlSetTermios(fd, ioctlSetTermios, old)
	buffer := make([]byte, 1)
	if _, err := in.Read(buffer); err != nil {
		return 0, err
	}
	return buffer[0], nil
}
