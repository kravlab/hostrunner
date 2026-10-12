package daemon

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// injectedFile returns a file holding content for a command to inherit:
// a memfd, so the content never reaches a disk, sealed so that neither
// the program nor anything it starts can change it, and positioned at its
// start. The program can seek in it and reopen it through its descriptor
// path. name only labels the memfd in /proc.
func injectedFile(name, content string) (*os.File, error) {
	fd, err := unix.MemfdCreate("hostrunner-"+name, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	f := os.NewFile(uintptr(fd), "hostrunner-"+name)
	if _, err := io.WriteString(f, content); err != nil {
		f.Close()
		return nil, err
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		f.Close()
		return nil, fmt.Errorf("seal: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
