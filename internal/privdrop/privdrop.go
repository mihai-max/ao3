// Package privdrop lets the container start as root, fix ownership of the
// index folder, and then run as PUID:PGID (Unraid uses 99:100).
package privdrop

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Drop switches to PUID/PGID when running as root and both are set. It
// chowns the index directory and the database files in it first, so an
// index created by an earlier root run stays writable.
func Drop(indexDir string) error {
	if os.Getuid() != 0 {
		return nil
	}
	us, gs := os.Getenv("PUID"), os.Getenv("PGID")
	if us == "" && gs == "" {
		return nil
	}
	uid, err := strconv.Atoi(defaultTo(us, "99"))
	if err != nil {
		return fmt.Errorf("PUID %q: %w", us, err)
	}
	gid, err := strconv.Atoi(defaultTo(gs, "100"))
	if err != nil {
		return fmt.Errorf("PGID %q: %w", gs, err)
	}
	if uid == 0 {
		return nil
	}
	if err := os.MkdirAll(indexDir, 0o775); err != nil {
		return err
	}
	if err := os.Lchown(indexDir, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", indexDir, err)
	}
	if entries, err := os.ReadDir(indexDir); err == nil {
		for _, e := range entries {
			os.Lchown(filepath.Join(indexDir, e.Name()), uid, gid)
		}
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	// Changing user makes Linux mark the process non-dumpable, which locks
	// it out of its own /proc/self/io; the upgrade progress bar reads that.
	// Only processes of the same user (rclone, in this container) gain the
	// right to inspect it.
	syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 1, 0)
	if um := os.Getenv("UMASK"); um != "" {
		if v, err := strconv.ParseUint(um, 8, 32); err == nil {
			syscall.Umask(int(v))
		}
	}
	return nil
}

func defaultTo(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
