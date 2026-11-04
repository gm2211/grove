//go:build darwin || linux

package secfile

import (
	"fmt"
	"os"
	"syscall"
)

// getuid is a variable so a test can pose as another user (chown needs root).
var getuid = os.Getuid

func checkOwnedByUs(path string, info os.FileInfo) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
		return fmt.Errorf("%s is owned by uid %d, not by this user (uid %d)", path, st.Uid, getuid())
	}
	return nil
}
