package worker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ioCopy copies src into dst.
func ioCopy(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// openAt opens a file relative to a directory, without following a symlink out of it.
func openAt(dirfd uintptr, name string) (int, error) {
	return syscall.Openat(int(dirfd), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

// The plan logins: the owner's own coding-CLI sign-in files, copied read-only into a job's private home.
//
// A CLI signs in with a file in the owner's home (~/.claude/.credentials.json, ~/.codex/auth.json,
// ~/.local/share/opencode/auth.json and the like). A job that runs on the owner's machine wants the plan the owner
// signed in for, but a job is code from a task: it must not be able to read the owner's files, and the Node must not
// send a credential anywhere. So the owner names the files (worker.plan_logins, off by default), the Node copies each
// one into that one job's home, read-only, and the copy goes with the job.
//
// Nothing about the content leaves the machine: the copy is made on the Node, it is never logged, never uploaded and
// never part of a result.

// planLoginName is the path of a plan login under the job's home: the path it has under the owner's home.
func planLoginName(home, path string) (string, error) {
	rel, err := filepath.Rel(home, filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("plan login %q is not under the home", path)
	}
	return rel, nil
}

// stagePlanLogins copies each named plan login into root, and returns the names (the paths under the job's home).
//
// A file that is not there is skipped: the CLI it belongs to is simply not signed in, which is not an error. A file
// that is there but cannot be read, or is not a plain file, fails the job: a half-copied credential is worse than none.
func stagePlanLogins(logins []string, root string) ([]string, error) {
	if len(logins) == 0 {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("the Node user's home is unknown: %w", err)
	}
	names := make([]string, 0, len(logins))
	for _, p := range logins {
		name, err := planLoginName(home, p)
		if err != nil {
			return nil, fmt.Errorf("plan login %q: %w", p, err)
		}
		st, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue // not signed in for this CLI
			}
			return nil, fmt.Errorf("plan login %q: %w", p, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("plan login %q is not a plain file", p)
		}
		src, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("plan login %q: %w", p, err)
		}
		dst := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			src.Close()
			return nil, fmt.Errorf("plan login %q: %w", p, err)
		}
		err = copyFile(dst, src)
		src.Close()
		if err != nil {
			return nil, fmt.Errorf("plan login %q: %w", p, err)
		}
		names = append(names, name)
	}
	return names, nil
}

// copyFile writes src into dst, read-only for its owner: a job may read the credential, never change it.
func copyFile(dst string, src *os.File) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o400)
	if err != nil {
		return err
	}
	if _, err := ioCopy(out, src); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o400)
}

// copyPlanLogins copies the staged plan logins from the job's root into its working directory (its home), read-only.
//
// The names are paths under the home: they may hold a directory (~/.claude/.credentials.json), so they are not the flat
// names the job's inputs use. A name that would leave the home is refused.
func copyPlanLogins(from *os.File, work string, names []string) error {
	for _, name := range names {
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("plan login name %q", name)
		}
		fd, err := openAt(from.Fd(), clean)
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
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o400)
			if err != nil {
				return err
			}
			if _, err := ioCopy(out, src); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			return os.Chmod(dst, 0o400)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
