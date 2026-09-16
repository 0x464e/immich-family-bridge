//go:build !windows

package httpapi

import (
	"os"
	"syscall"
)

func inodeDetails(info os.FileInfo) map[string]any {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return map[string]any{"device": st.Dev, "inode": st.Ino, "linkCount": st.Nlink}
	}
	return nil
}
