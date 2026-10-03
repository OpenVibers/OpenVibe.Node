//go:build linux

package worker

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// A job starts as the Node's own executable (/proc/self/exe) with argv[0] sandboxArg0 and its sandboxSpec as argv[1],
// already in its namespaces and its cgroup, holding CAP_SYS_ADMIN of its user namespace only (an ambient capability).
// It builds the job's private root, sets the rlimits, gives up every capability, sets no_new_privs and the seccomp
// filter, and only then executes the function. It reports a failure on statusFD as text. Once confined it writes one
// NUL byte there and waits for one byte on goFD: the Node checks the confinement and sets RLIMIT_CPU meanwhile, while
// the process is sure to be alive. The function's execve then closes statusFD. artifactFD is the function's artifact
// directory, opened by the Node, when the init is to copy it.
const (
	sandboxArg0 = "openvibe-node-sandbox"
	statusFD    = 4 // fd 3 is the function's result
	goFD        = 5
	artifactFD  = 6
)

// sandboxSpec is everything the sandbox init needs, from the Node.
type sandboxSpec struct {
	Root  string     `json:"root"` // the host directory the private root is mounted on
	Work  string     `json:"work"` // the working directory, inside the private root
	Disk  int64      `json:"disk"` // the size of the job's /tmp, and the most its artifact's copy may take
	Binds []bindSpec `json:"binds"`
	// Artifact is where the copy of the artifact directory open on artifactFD goes (its host path); "" for none.
	Artifact string     `json:"artifact,omitempty"`
	Links    []linkSpec `json:"links"`
	Limits   []rlimit   `json:"limits"`
	Argv     []string   `json:"argv"`
	Env      []string   `json:"env"`
}

// bindSpec bind-mounts the host's path at the same path in the private root, read-only unless it is a device node.
// The bind is made by path in the job's own mount namespace: a mount cannot be bound from another namespace.
type bindSpec struct {
	Path string `json:"path"`
	Dev  bool   `json:"dev,omitempty"`
}

type linkSpec struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type rlimit struct {
	Res int    `json:"res"`
	Cur uint64 `json:"cur"`
	Max uint64 `json:"max"`
}

// The rlimit resources (the same numbers on amd64 and arm64; seccomp covers no other architecture).
const (
	rlimitCPU    = 0
	rlimitFSIZE  = 1
	rlimitCORE   = 4
	rlimitNPROC  = 6
	rlimitNOFILE = 7
	rlimitAS     = 9
)

func init() {
	if len(os.Args) == 2 && os.Args[0] == sandboxArg0 {
		sandboxMain(os.Args[1])
	}
}

// sandboxMain never returns: it executes the function or exits 126 after writing why it could not to statusFD.
func sandboxMain(arg string) {
	// no_new_privs, the seccomp filter and the capabilities belong to this thread, the one that calls execve.
	runtime.LockOSThread()
	err := enterSandbox(arg)
	status := os.NewFile(statusFD, "status")
	_, _ = status.WriteString(err.Error())
	os.Exit(126)
}

func enterSandbox(arg string) error {
	var s sandboxSpec
	if err := json.Unmarshal([]byte(arg), &s); err != nil {
		return err
	}
	if len(s.Argv) == 0 {
		return errors.New("no command")
	}
	filter, err := seccompFilter()
	if err != nil {
		return err
	}
	step := func(what string, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	r := s.Root
	if err := step("making mounts private", syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, "")); err != nil {
		return err
	}
	// The private root holds the copies of the CA certificates: a few hundred KiB on a common system.
	if err := step("mounting the private root", syscall.Mount("tmpfs", r, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0755,size=16m")); err != nil {
		return err
	}
	if err := os.Mkdir(r+"/tmp", 0o755); err != nil {
		return err
	}
	if err := step("mounting /tmp", syscall.Mount("tmpfs", r+"/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV,
		"mode=0700,size="+strconv.FormatInt(s.Disk, 10))); err != nil {
		return err
	}
	if err := os.MkdirAll(r+s.Work, 0o700); err != nil {
		return err
	}
	for _, b := range s.Binds {
		if err := step("bind-mounting "+b.Path, bind(r, b)); err != nil {
			return err
		}
	}
	for _, l := range s.Links {
		if err := os.MkdirAll(filepath.Dir(r+l.Path), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(l.Target, r+l.Path); err != nil {
			return err
		}
	}
	if err := step("copying the CA certificates", stageCA(r, caPaths)); err != nil {
		return err
	}
	var artifactDev uint64
	if s.Artifact != "" {
		if artifactDev, err = stageArtifact(r, s.Artifact, s.Disk); err != nil {
			return step("copying the artifact directory "+s.Artifact, err)
		}
	}
	if err := os.Mkdir(r+"/proc", 0o755); err != nil {
		return err
	}
	if err := step("mounting /proc", syscall.Mount("proc", r+"/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")); err != nil {
		return err
	}
	// The private root becomes /; the host's root, stacked on it by pivot_root, is detached with everything under it.
	if err := step("entering the private root", syscall.Chdir(r)); err != nil {
		return err
	}
	if err := step("pivot_root", syscall.PivotRoot(".", ".")); err != nil {
		return err
	}
	if err := step("detaching the host's root", syscall.Unmount(".", syscall.MNT_DETACH)); err != nil {
		return err
	}
	if err := step("making / read-only", syscall.Mount("", "/", "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|
		syscall.MS_NOSUID|syscall.MS_NODEV, "")); err != nil {
		return err
	}
	// The job's own path to its artifact directory must lead to the copy, never to the host's directory under it. The
	// Node checked that no host user but root or the Node's own can change that path while the job runs (checkRoute).
	if s.Artifact != "" {
		var st syscall.Stat_t
		if err := syscall.Stat(s.Artifact, &st); err != nil || st.Dev != artifactDev {
			return fmt.Errorf("the artifact directory %s does not lead to its copy in the private root (%v)", s.Artifact, err)
		}
	}
	if err := syscall.Chdir(s.Work); err != nil {
		return err
	}
	// Only fds 0-3 reach the function; statusFD closes when execve succeeds.
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, e := range ents {
		if fd, err := strconv.Atoi(e.Name()); err == nil && fd > 3 {
			syscall.CloseOnExec(fd)
		}
	}
	// execve's arguments are made before the rlimits: past RLIMIT_AS this process may not be able to allocate.
	path, err := syscall.BytePtrFromString(s.Argv[0])
	if err != nil {
		return err
	}
	argv, err := syscall.SlicePtrFromStrings(s.Argv)
	if err != nil {
		return err
	}
	envv, err := syscall.SlicePtrFromStrings(s.Env)
	if err != nil {
		return err
	}
	for _, l := range s.Limits {
		lim := syscall.Rlimit{Cur: l.Cur, Max: l.Max}
		if _, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, 0, uintptr(l.Res), uintptr(unsafe.Pointer(&lim)), 0, 0, 0); e != 0 {
			return fmt.Errorf("rlimit %d: %w", l.Res, e)
		}
	}
	const (
		prCapAmbient, prCapAmbientClearAll = 47, 4
		prSetNoNewPrivs                    = 38
		prSetSeccomp, seccompModeFilter    = 22, 2
	)
	// The function runs as a non-zero uid of its user namespace: with no ambient capability and no file
	// capabilities (no_new_privs), execve leaves it none.
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("dropping the ambient capabilities: %w", e)
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	prog := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&prog)), 0, 0, 0); e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	runtime.KeepAlive(filter)
	var b [1]byte
	if _, err := syscall.Write(statusFD, b[:]); err != nil {
		return fmt.Errorf("reporting ready: %w", err)
	}
	if n, err := syscall.Read(goFD, b[:]); n != 1 {
		return fmt.Errorf("the Node did not let the function start (%v)", err)
	}
	_, _, e := syscall.RawSyscall(syscall.SYS_EXECVE, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&argv[0])),
		uintptr(unsafe.Pointer(&envv[0])))
	return fmt.Errorf("executing %s: %w", s.Argv[0], e)
}

// bind mounts b under the private root r, with every mount under it (a mount nested in /usr or the artifact
// directory included) read-only, nosuid and nodev: mount_setattr with AT_RECURSIVE adds those flags to the whole
// tree and leaves the others (noexec, atime, which the kernel may have locked) as they are.
func bind(r string, b bindSpec) error {
	dst := r + b.Path
	fi, err := os.Stat(b.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if fi.IsDir() {
		if err := os.Mkdir(dst, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	} else if f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		return err
	} else {
		f.Close()
	}
	if err := syscall.Mount(b.Path, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return err
	}
	if b.Dev {
		return nil
	}
	return readOnlyTree(dst)
}

// stageArtifact copies the artifact directory open on artifactFD, the host's src, into a tmpfs of at most size bytes
// mounted where src leads in the private root r, and makes it read-only: the job sees the tree as it was when it
// started, never what the host adds to it later (a socket or a FIFO a host process could be reached through, say).
// That holds in a system path too: the copy covers what the bind of /usr (say) shows there, on a path to it the Node
// checked that only root or the Node's own user can change (checkRoute). It returns the copy's
// device number, for the job to check that its path to the artifact leads there. The copy is read as the job's own
// user, who must be able to reach src by its path (search every directory above it) and list it, and holds
// directories, regular files and symlinks only, with their permission bits and modification times (no owner, set-id
// or sticky bit); what the job's user may not read is left out; anything else in the tree, or a mount nested in it (a
// bind mount of the same filesystem included), fails the job. The copy counts towards the job's memory and its reads
// towards its io.max.
func stageArtifact(r, src string, size int64) (uint64, error) {
	// Opened again by its path, as the job's user: the Node opened it with its own rights (root's, with run_as), and
	// reopening that descriptor through /proc would skip the search permission of the directories above it.
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var st, node syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	if err := syscall.Fstat(artifactFD, &node); err != nil {
		return 0, err
	}
	syscall.Close(artifactFD)
	if st.Dev != node.Dev || st.Ino != node.Ino {
		return 0, errors.New("it changed since the Node checked it")
	}
	dst, err := inRoot(r, src)
	if err != nil {
		return 0, err
	}
	if err := syscall.Mount("tmpfs", dst, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV,
		"mode=0700,size="+strconv.FormatInt(size, 10)); err != nil {
		return 0, err
	}
	var copied syscall.Stat_t
	if err := syscall.Stat(dst, &copied); err != nil {
		return 0, err
	}
	if err := copyDir(f, dst); err != nil {
		return 0, err
	}
	if err := chmodTimes(dst, st); err != nil {
		return 0, err
	}
	return copied.Dev, readOnlyTree(dst)
}

// inRoot returns the host path, free of symlinks, of the directory path leads to in the private root r (made if it
// is missing): a symlink on the way, as /lib to /usr/lib or one in a bound system path, is followed as the job will
// follow it, absolute or not, and never out of r.
func inRoot(r, path string) (string, error) {
	const resolveNoMagiclinks, resolveInRoot = 0x02, 0x10
	root, err := syscall.Open(r, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer syscall.Close(root)
	open := func() (int, error) {
		return openat2(root, path, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, resolveNoMagiclinks|resolveInRoot)
	}
	fd, err := open()
	if errors.Is(err, syscall.ENOENT) {
		if err := os.MkdirAll(r+path, 0o755); err != nil {
			return "", err
		}
		fd, err = open()
	}
	if err != nil {
		return "", err
	}
	defer syscall.Close(fd)
	p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(p, r+"/") {
		return "", fmt.Errorf("%s leads to %s, not into the private root", path, p)
	}
	return p, nil
}

// maxCAFile is the largest CA certificate file copied (a bundle of every public CA is about 250 KiB).
const maxCAFile = 4 << 20

// stageCA copies the CA certificates at paths (those that exist) to the same paths under the private root r, as the
// job's user, and nothing else: of a regular file, only the PEM CERTIFICATE blocks that parse as X.509 certificates,
// written again without their headers and whatever else lay between them (a private key filed with them, say); a file
// with none, one the job's user may not read and anything but a directory, a regular file or a symlink are left out.
// Directories are walked whatever is mounted in them. A symlink is copied as it is: it leads only to what the private
// root holds (the copied certificates or the system paths). The copies are read-only with the rest of the root.
func stageCA(r string, paths []string) error {
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(r+p), 0o755); err != nil {
			return err
		}
		if err := copyCA(p, r+p); err != nil {
			return err
		}
	}
	return nil
}

func copyCA(src, dst string) error {
	fi, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return nil
	} else if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(t, dst)
	case fi.IsDir():
		f, err := os.Open(src)
		if errors.Is(err, fs.ErrPermission) {
			return nil
		} else if err != nil {
			return err
		}
		names, err := f.Readdirnames(-1)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		if err := os.Mkdir(dst, 0o755); err != nil {
			return err
		}
		for _, n := range names {
			if err := copyCA(src+"/"+n, dst+"/"+n); err != nil {
				return err
			}
		}
	case fi.Mode().IsRegular():
		certs, err := publicCerts(src)
		if err != nil || len(certs) == 0 {
			return err
		}
		w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = w.Write(certs)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return nil
}

// publicCerts returns the certificates of the PEM file path (see stageCA); nil if it holds none, if it is no longer a
// regular file, or if the job's user may not read it.
func publicCerts(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ELOOP) || errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCAFile+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(b) > maxCAFile {
		return nil, nil
	}
	var out []byte
	for {
		var blk *pem.Block
		if blk, b = pem.Decode(b); blk == nil {
			return out, nil
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err == nil {
			out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: blk.Bytes})...)
		}
	}
}

// oPath is O_PATH (the same on amd64 and arm64): it opens a name without opening what it names, so no FIFO or device
// is opened while the copy checks what each entry is.
const oPath = 0x200000

// copyDir copies the entries of the directory dir into the directory dst.
func copyDir(dir *os.File, dst string) error {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := copyEntry(int(dir.Fd()), n, dst+"/"+n); err != nil {
			return err
		}
	}
	return nil
}

// openEntry opens the entry name of the directory dir O_PATH (a symlink itself, not what it points to) with openat2,
// which fails EXDEV if the entry is a mount point: unlike a device number, that also catches a bind mount of the
// artifact's own filesystem (of a Node directory, say).
func openEntry(dir int, name string) (int, error) {
	const resolveNoXdev, resolveNoSymlinks, resolveBeneath = 0x01, 0x04, 0x08
	return openat2(dir, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, resolveNoXdev|resolveNoSymlinks|resolveBeneath)
}

// openat2 opens name, relative to the directory dir, with the open flags and the RESOLVE_* flags resolve.
func openat2(dir int, name string, flags, resolve uint64) (int, error) {
	const sysOpenat2 = 437 // the same on amd64 and arm64
	how := struct{ flags, mode, resolve uint64 }{flags: flags, resolve: resolve}
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return -1, err
	}
	fd, _, e := syscall.Syscall6(sysOpenat2, uintptr(dir), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&how)),
		unsafe.Sizeof(how), 0, 0)
	if e != 0 {
		return -1, e
	}
	return int(fd), nil
}

// copyEntry copies the entry name of the directory open on dir to dst. It opens the entry O_PATH, checks what it is
// on that descriptor, and opens a directory or a regular file for reading through it (/proc/self/fd), so what it
// reads is what it checked even if the host swaps the entry meanwhile.
func copyEntry(dir int, name, dst string) error {
	fd, err := openEntry(dir, name)
	if errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("%s is a mount point: a mount nested in the artifact directory is not copied", dst)
	} else if err != nil {
		return fmt.Errorf("%s: %w", dst, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	reopen := func(flags int) (int, error) {
		return syscall.Open("/proc/self/fd/"+strconv.Itoa(fd), flags|syscall.O_CLOEXEC, 0)
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFLNK:
		var buf [4096]byte
		empty := [1]byte{}
		n, _, e := syscall.Syscall6(syscall.SYS_READLINKAT, uintptr(fd), uintptr(unsafe.Pointer(&empty[0])),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		if e != 0 {
			return fmt.Errorf("%s: %w", dst, e)
		}
		return os.Symlink(string(buf[:n]), dst)
	case syscall.S_IFDIR:
		d, err := reopen(syscall.O_RDONLY | syscall.O_DIRECTORY)
		if errors.Is(err, syscall.EACCES) {
			return nil // the job's user may not list it: it is left out
		} else if err != nil {
			return fmt.Errorf("%s: %w", dst, err)
		}
		sub := os.NewFile(uintptr(d), dst)
		defer sub.Close()
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		if err := copyDir(sub, dst); err != nil {
			return err
		}
	case syscall.S_IFREG:
		r, err := reopen(syscall.O_RDONLY)
		if errors.Is(err, syscall.EACCES) {
			return nil // the job's user may not read it: it is left out
		} else if err != nil {
			return fmt.Errorf("%s: %w", dst, err)
		}
		src := os.NewFile(uintptr(r), dst)
		defer src.Close()
		w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", dst, err)
		}
	default:
		return fmt.Errorf("%s is neither a directory, a regular file nor a symlink (a socket, FIFO or device)", dst)
	}
	return chmodTimes(dst, st)
}

// chmodTimes gives the copy dst the permission bits and the times of the original st.
func chmodTimes(dst string, st syscall.Stat_t) error {
	if err := os.Chmod(dst, os.FileMode(st.Mode&0o777)); err != nil {
		return err
	}
	return syscall.UtimesNano(dst, []syscall.Timespec{st.Atim, st.Mtim})
}

// readOnlyTree makes the mount at path and every mount under it read-only, nosuid and nodev.
func readOnlyTree(path string) error {
	const (
		sysMountSetattr                                  = 442 // the same on amd64 and arm64
		atRecursive                                      = 0x8000
		mountAttrRdonly, mountAttrNosuid, mountAttrNodev = 0x1, 0x2, 0x4
	)
	attr := struct{ set, clr, propagation, userns uint64 }{set: mountAttrRdonly | mountAttrNosuid | mountAttrNodev}
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	const atFdcwd = -100 // AT_FDCWD (the syscall package does not export it)
	cwd := atFdcwd
	if _, _, e := syscall.Syscall6(sysMountSetattr, uintptr(cwd), uintptr(unsafe.Pointer(p)), atRecursive,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0); e != 0 {
		return fmt.Errorf("mount_setattr: %w", e)
	}
	return nil
}

// cloneNamespaces are the clone flags that make a namespace; a job may not use them.
const cloneNamespaces = syscall.CLONE_NEWNS | syscall.CLONE_NEWCGROUP | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC |
	syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNET

// socketFamilies are the only socket families a job may open: Unix, IPv4, IPv6 and netlink (all scoped by its
// network namespace; vsock, for one, is not).
var socketFamilies = []uint32{syscall.AF_UNIX, syscall.AF_INET, syscall.AF_INET6, syscall.AF_NETLINK}

// seccompFilter is the job's seccomp program: another architecture's system calls kill the process; clone3 fails
// ENOSYS (libc falls back to clone); clone with a namespace flag and socket of another family fail; every call on the
// allowlist passes; anything else fails EPERM (mount, unshare, setns, ptrace, bpf, keyctl, io_uring, perf_event_open,
// kernel modules, ...).
func seccompFilter() ([]syscall.SockFilter, error) {
	if len(allowedSyscalls) == 0 {
		return nil, fmt.Errorf("no seccomp allowlist for %s", runtime.GOARCH)
	}
	const (
		ldAbs, jeq, jge, jset, ret  = 0x20, 0x15, 0x35, 0x45, 0x06
		retKill, retAllow, retErrno = 0x80000000, 0x7fff0000, 0x00050000
		offNr, offArch, offArg0     = 0, 4, 16 // in struct seccomp_data; arg0's low 32 bits on little-endian
	)
	stmt := func(code uint16, k uint32) syscall.SockFilter { return syscall.SockFilter{Code: code, K: k} }
	jump := func(k uint32, jt, jf uint8) syscall.SockFilter {
		return syscall.SockFilter{Code: jeq, Jt: jt, Jf: jf, K: k}
	}
	errno := func(e syscall.Errno) syscall.SockFilter { return stmt(ret, retErrno|uint32(e)) }
	p := []syscall.SockFilter{stmt(ldAbs, offArch), jump(auditArch, 1, 0), stmt(ret, retKill), stmt(ldAbs, offNr)}
	if x32ABI {
		p = append(p, syscall.SockFilter{Code: jge, Jt: 0, Jf: 1, K: 0x40000000}, errno(syscall.EPERM))
	}
	p = append(p, jump(sysClone3, 0, 1), errno(syscall.ENOSYS),
		jump(sysClone, 0, 4), stmt(ldAbs, offArg0), syscall.SockFilter{Code: jset, Jt: 0, Jf: 1, K: cloneNamespaces},
		errno(syscall.EPERM), stmt(ret, retAllow))
	n := len(socketFamilies)
	p = append(p, jump(sysSocket, 0, uint8(n+3)), stmt(ldAbs, offArg0))
	for i, f := range socketFamilies {
		p = append(p, jump(f, uint8(n-i), 0))
	}
	p = append(p, errno(syscall.EAFNOSUPPORT), stmt(ret, retAllow))
	for _, nr := range allowedSyscalls {
		p = append(p, jump(nr, 0, 1), stmt(ret, retAllow))
	}
	return append(p, errno(syscall.EPERM)), nil
}

// The read-only system paths of every private root (symlinks are recreated, as on merged-/usr systems), the few
// files of /etc programs need to start, the device nodes, and copies of the public CA certificates (stageCA). Nothing
// else of the host is there: no /home, /root, /var, /run, /srv, /mnt, /media, /sys, no rest of /etc (no
// /etc/ssl/private, /etc/shadow nor any other key), and none of the Node's directories.
var (
	systemPaths = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"}
	etcPaths    = []string{"/etc/alternatives", "/etc/group", "/etc/hosts", "/etc/ld.so.cache", "/etc/ld.so.conf",
		"/etc/ld.so.conf.d", "/etc/localtime", "/etc/mime.types", "/etc/nsswitch.conf", "/etc/passwd"}
	// caPaths are where Debian, Fedora, Alpine, Arch and OpenSSL keep the public CA certificates: the directories that
	// hold certificates (never their parent: /etc/ssl and /etc/pki/tls also hold private keys) and the bundles. They
	// are copied, certificates only, never bound: a key or a mount filed among them would be seen whole.
	caPaths = []string{"/etc/ssl/certs", "/etc/ssl/cert.pem", "/etc/ssl/ca-bundle.pem", "/etc/pki/tls/certs",
		"/etc/pki/tls/cacert.pem", "/etc/pki/ca-trust/extracted", "/etc/ca-certificates/extracted"}
	devPaths = []string{"/dev/full", "/dev/null", "/dev/random", "/dev/urandom", "/dev/zero"}
	devLinks = []linkSpec{{"/dev/fd", "/proc/self/fd"}, {"/dev/stdin", "/proc/self/fd/0"},
		{"/dev/stdout", "/proc/self/fd/1"}, {"/dev/stderr", "/proc/self/fd/2"}, {"/dev/shm", "/tmp"}}
	// hiddenPaths are host paths the probe checks no job can see.
	hiddenPaths = []string{"/var", "/etc/shadow", "/etc/ssl/private", "/etc/pki/tls/private"}
)

// hostBind is a host path to bind-mount at the same path in the private root.
type hostBind struct {
	path string
	dev  bool
}

// rootPlan lists what of the host every job sees bound: the system paths, the /etc files and the devices. It refuses
// any of them that is, holds or lies in one of the Node's directories or hiddenPaths, as given or where its symlinks
// lead. The CA certificates and a function's artifact directory, wherever it is, are not bound but copied (stageCA,
// stageArtifact).
func rootPlan(nodeDirs []string) (binds []hostBind, links []linkSpec, err error) {
	for _, p := range systemPaths {
		fi, err := os.Lstat(p)
		switch {
		case err != nil:
			continue
		case fi.Mode()&os.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return nil, nil, err
			}
			links = append(links, linkSpec{p, t})
		case fi.IsDir():
			binds = append(binds, hostBind{path: p})
		}
	}
	for _, p := range etcPaths {
		if _, err := os.Stat(p); err == nil {
			binds = append(binds, hostBind{path: p})
		}
	}
	for _, b := range binds {
		if d, ok := overlaps(b.path, nodeDirs); ok {
			return nil, nil, fmt.Errorf("%s would expose the Node's directory %s to jobs", b.path, d)
		}
		if d, ok := overlaps(b.path, hiddenPaths); ok {
			return nil, nil, fmt.Errorf("%s would expose the host's %s to jobs", b.path, d)
		}
	}
	for _, p := range devPaths {
		binds = append(binds, hostBind{path: p, dev: true})
	}
	// Parents before what they hold.
	sort.SliceStable(binds, func(i, j int) bool { return strings.Count(binds[i].path, "/") < strings.Count(binds[j].path, "/") })
	return binds, append(links, devLinks...), nil
}

// An artifact directory may not be, hold or lie in hostOnlyPaths (kernel interfaces, the host's runtime sockets, its
// configuration and secrets), nor be or hold sharedPaths (it may lie inside one, as a build directory in /tmp does).
var (
	hostOnlyPaths = []string{"/proc", "/sys", "/dev", "/run", "/var/run", "/var/lock", "/etc", "/boot", "/root"}
	sharedPaths   = []string{"/tmp", "/var/tmp", "/home"}
)

// checkArtifact refuses an artifact directory that would show the job host paths beyond the function's own files:
// one in or over hostOnlyPaths, one over sharedPaths, one that is, holds or lies in one of the Node's directories.
func checkArtifact(nodeDirs []string, artifact string) error {
	if d, ok := overlaps(artifact, hostOnlyPaths); ok {
		return fmt.Errorf("artifact directory %s would expose the host's %s to jobs", artifact, d)
	}
	for _, d := range sharedPaths {
		for _, a := range resolved(artifact) {
			for _, b := range resolved(d) {
				if a == "/" || under(b, []string{a}) {
					return fmt.Errorf("artifact directory %s would expose the host's %s to jobs", artifact, d)
				}
			}
		}
	}
	if d, ok := overlaps(artifact, nodeDirs); ok {
		return fmt.Errorf("artifact directory %s would expose the Node's directory %s to jobs", artifact, d)
	}
	return nil
}

// scanArtifact refuses, at boot, an artifact directory whose tree holds what its copy would not: a Unix socket, a
// FIFO, a device. Every job's copy refuses them again (stageArtifact): this only says so before any job runs.
func scanArtifact(artifact string) error {
	root, err := filepath.EvalSymlinks(artifact)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing to copy; the job fails
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("artifact directory %s: %w", artifact, err)
		}
		if d.Type()&(fs.ModeSocket|fs.ModeNamedPipe|fs.ModeDevice|fs.ModeCharDevice) != 0 {
			return fmt.Errorf("artifact directory %s holds %s, a socket, FIFO or device jobs could reach the host through",
				artifact, p)
		}
		return nil
	})
}

// checkRoute refuses an artifact directory whose path, followed as the job follows it in its private root (binds and
// links as rootPlan makes them), crosses a directory of a bound host path that a host user other than one of trusted
// could change: one owned by another user, writable by a group other than root's, or writable by others. In such a
// directory a host user could, once the job runs, swap a symlink on the path or rename a directory on it (the copy's
// mount goes with it) and leave the job's path to its artifact leading to a live host directory, a socket or FIFO
// added there included. The rest of the path, outside the bound paths, is the private root's own.
func checkRoute(binds []hostBind, links []linkSpec, artifact string, trusted []uint32) error {
	var bound []string
	for _, b := range binds {
		bound = append(bound, b.path)
	}
	linked := map[string]string{}
	for _, l := range links {
		linked[l.Path] = l.Target
	}
	cur, rest, hops := "/", strings.Split(artifact, "/"), 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		target, isLink := linked[next]
		if under(cur, bound) {
			var st syscall.Stat_t
			if err := syscall.Stat(cur, &st); err != nil {
				return fmt.Errorf("artifact directory %s: %s: %w", artifact, cur, err)
			}
			if !slices.Contains(trusted, st.Uid) || st.Mode&0o002 != 0 || st.Mode&0o020 != 0 && st.Gid != 0 {
				return fmt.Errorf("artifact directory %s: its path crosses %s, which a host user could change while "+
					"a job runs (owner %d, mode %#o): its directories in a system path must be root's and writable by "+
					"root only", artifact, cur, st.Uid, st.Mode&0o7777)
			}
			fi, err := os.Lstat(next)
			if err != nil {
				return fmt.Errorf("artifact directory %s: %w", artifact, err)
			}
			if isLink = fi.Mode()&fs.ModeSymlink != 0; isLink {
				if target, err = os.Readlink(next); err != nil {
					return fmt.Errorf("artifact directory %s: %w", artifact, err)
				}
			}
		}
		if !isLink {
			cur = next
			continue
		}
		if hops++; hops > 40 {
			return fmt.Errorf("artifact directory %s: %w", artifact, syscall.ELOOP)
		}
		if filepath.IsAbs(target) {
			cur = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return nil
}

// trustedUIDs are the host users who may change the path to an artifact directory (checkRoute): root and the Node's.
func trustedUIDs() []uint32 { return []uint32{0, uint32(os.Geteuid())} }

// openArtifact opens the artifact directory a job's sandbox init copies, and checks the directory it opened (where
// its path leads now, whatever it led to at boot) as checkArtifact does, and its path as checkRoute does: the init
// copies from this descriptor, not by path. nil: nothing to copy.
func openArtifact(nodeDirs []string, binds []hostBind, links []linkSpec, artifact string) (*os.File, error) {
	if artifact == "" {
		return nil, nil
	}
	f, err := os.OpenFile(artifact, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("artifact directory: %w", err)
	}
	real, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err == nil {
		err = checkArtifact(nodeDirs, artifact)
	}
	if err == nil {
		err = checkArtifact(nodeDirs, real)
	}
	if err == nil {
		err = checkRoute(binds, links, artifact, trustedUIDs())
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// resolved is p cleaned and, if it differs, with its symlinks resolved.
func resolved(p string) []string {
	ps := []string{filepath.Clean(p)}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != ps[0] {
		ps = append(ps, r)
	}
	return ps
}

// writableMounts lists the mounts of the process pid that are not read-only, except its /tmp, its /proc and the
// device nodes: what a job could write to besides its own scratch space.
func writableMounts(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/mountinfo")
	if err != nil {
		return nil, err
	}
	var rw []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l) // f[4] is the mount point, relative to the process's root; f[5] its mount options
		if len(f) < 6 {
			return nil, fmt.Errorf("malformed mountinfo line %q", l)
		}
		if f[4] == "/tmp" || f[4] == "/proc" || slices.Contains(devPaths, f[4]) {
			continue
		}
		if !slices.Contains(strings.Split(f[5], ","), "ro") {
			rw = append(rw, f[4])
		}
	}
	return rw, nil
}

// under reports whether p is one of dirs or lies inside one.
func under(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// overlaps returns the first of dirs that p is, holds or lies in, comparing both as given and with symlinks resolved.
func overlaps(p string, dirs []string) (string, bool) {
	ps := resolved(p)
	for _, d := range dirs {
		for _, a := range ps {
			for _, b := range resolved(d) {
				if a == "/" || b == "/" || under(a, []string{b}) || under(b, []string{a}) {
					return d, true
				}
			}
		}
	}
	return "", false
}
