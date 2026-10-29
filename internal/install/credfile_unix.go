//go:build darwin || linux

package install

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwnedByUs refuses a path another user owns: replacing or trusting it would put a secret
// where that user can read it.
func checkOwnedByUs(path string, info os.FileInfo) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not by this user (uid %d)", path, st.Uid, os.Getuid())
	}
	return nil
}
