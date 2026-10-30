//go:build linux

package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// copyPlanLogins copies the staged plan logins from the job's root into its working directory (its home), read-only.
//
// The names are paths under the home: they may hold a directory (~/.claude/.credentials.json), so they are not the flat
// names the job's inputs use. A name that would leave the home is refused, and the file is opened without following a
// symlink out of the root.
func copyPlanLogins(from *os.File, work string, names []string) error {
	for _, name := range names {
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("plan login name %q", name)
		}
		fd, err := syscall.Openat(int(from.Fd()), clean, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", clean, err)
		}
		src := os.NewFile(uintptr(fd), clean)
		err = func() error {
			defer src.Close()
			if st, err := src.Stat(); err != nil || !st.Mode().IsRegular() {
				return fmt.Errorf("%s is not a plain file (%v)", clean, err)
			}
			dst := filepath.Join(work, clean)
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			return copyFile(dst, src)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
