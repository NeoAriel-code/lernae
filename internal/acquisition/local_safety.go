package acquisition

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// These small provider-local helpers follow agentops/safety.go's os.Root,
// no-follow openat, descriptor identity, and no-replace publication patterns.
// Agent Cache's exported operations create Asset namespaces and require Asset
// restore plans; importing that lifecycle here would grant unrelated authority.
var ErrLocalRoots = errors.New("local acquisition requires separate safe existing source and private staging directories")

// ValidateLocalRoots is lexical validation only. OpenLocal additionally checks
// every actual component and retains its authority. No configured root is made
// automatically: configuration never creates an arbitrary filesystem subtree.
func ValidateLocalRoots(source, staging string) error {
	if err := ValidateLocalStagingRoot(staging); err != nil {
		return err
	}
	for _, path := range []string{source, staging} {
		if !filepath.IsAbs(path) || path != filepath.Clean(path) || path == "/" || strings.ContainsAny(path, "\x00\\") {
			return ErrLocalRoots
		}
	}
	for _, pair := range [][2]string{{source, staging}, {staging, source}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, "../")) {
			return ErrLocalRoots
		}
	}
	return nil
}

// ValidateLocalStagingRoot reserves the exact case-sensitive Agent namespace
// component anywhere in a configured staging path. This conservative naming
// restriction does not infer a remote Agent's cache or require shared mounts.
// Config also calls it for disabled/partial settings with a nonempty staging root.
func ValidateLocalStagingRoot(staging string) error {
	if !filepath.IsAbs(staging) || staging != filepath.Clean(staging) || staging == "/" || strings.ContainsAny(staging, "\x00\\") {
		return ErrLocalRoots
	}
	for _, component := range strings.Split(staging, "/") {
		if component == "assets" {
			return ErrLocalRoots
		}
	}
	return nil
}

// CheckLocalRoots validates actual authority without creating provider state.
// Configuration uses it before enabling; Server opens retained handles later.
func CheckLocalRoots(source, staging string) error {
	if err := ValidateLocalRoots(source, staging); err != nil {
		return err
	}
	first, err := localOpenAuthority(source, false)
	if err != nil {
		return ErrLocalRoots
	}
	defer first.close()
	second, err := localOpenAuthority(staging, true)
	if err != nil {
		return ErrLocalRoots
	}
	defer second.close()
	if !localAuthoritiesSeparate(first, second) || !first.same() || !second.same() {
		return ErrLocalRoots
	}
	return nil
}

func localAuthoritiesSeparate(first, second *localAuthority) bool {
	for _, d := range first.directories {
		if os.SameFile(d.info, second.last().info) {
			return false
		}
	}
	for _, d := range second.directories {
		if os.SameFile(d.info, first.last().info) {
			return false
		}
	}
	return true
}

type localDirectory struct {
	root *os.Root
	file *os.File
	info os.FileInfo
}

func (d *localDirectory) close() {
	if d == nil {
		return
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	if d.root != nil {
		_ = d.root.Close()
	}
}

func localOpenAt(parent *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, ErrProvider
	}
	return os.NewFile(uintptr(fd), "local acquisition handle"), nil
}

func localDirectorySafe(info os.FileInfo, ancestor, private bool) bool {
	if info == nil || !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	uid := uint32(os.Geteuid())
	if ancestor {
		return (stat.Uid == uid || stat.Uid == 0) && (info.Mode().Perm()&0022 == 0 || (stat.Uid == 0 && info.Mode()&os.ModeSticky != 0))
	}
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	return stat.Uid == uid && info.Mode().Perm()&mask == 0
}

func localChild(parent *localDirectory, name string, private bool) (*localDirectory, error) {
	if !localComponent(name) {
		return nil, ErrProvider
	}
	info, err := parent.root.Lstat(name)
	if err != nil || !localDirectorySafe(info, false, private) {
		return nil, ErrProvider
	}
	file, err := localOpenAt(parent.file, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, ErrProvider
	}
	root, err := parent.root.OpenRoot(name)
	if err != nil {
		_ = file.Close()
		return nil, ErrProvider
	}
	child := &localDirectory{root: root, file: file, info: info}
	if !localChildSame(parent, name, child, private) {
		child.close()
		return nil, ErrProvider
	}
	return child, nil
}

func localChildSame(parent *localDirectory, name string, child *localDirectory, private bool) bool {
	current, err := parent.root.Lstat(name)
	opened, fdErr := child.file.Stat()
	rooted, rootErr := child.root.Stat(".")
	return err == nil && fdErr == nil && rootErr == nil && localDirectorySafe(current, false, private) &&
		os.SameFile(child.info, current) && os.SameFile(current, opened) && os.SameFile(opened, rooted)
}

func localComponent(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) && !strings.ContainsAny(name, "/\\\x00")
}

type localAuthority struct {
	directories []*localDirectory
	names       []string
	private     bool
}

func localOpenAuthority(path string, private bool) (*localAuthority, error) {
	root, err := os.OpenRoot("/")
	if err != nil {
		return nil, ErrLocalRoots
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = root.Close()
		return nil, ErrLocalRoots
	}
	file := os.NewFile(uintptr(fd), "local acquisition root authority")
	info, err := file.Stat()
	if err != nil {
		_ = root.Close()
		_ = file.Close()
		return nil, ErrLocalRoots
	}
	authority := &localAuthority{directories: []*localDirectory{{root: root, file: file, info: info}}, private: private}
	fail := func() (*localAuthority, error) { authority.close(); return nil, ErrLocalRoots }
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, name := range parts {
		parent := authority.last()
		parentInfo, err := parent.file.Stat()
		if err != nil || !localDirectorySafe(parentInfo, true, false) {
			return fail()
		}
		// Ancestors may be root-owned; final roots must be effective-user owned.
		entry, err := parent.root.Lstat(name)
		if err != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return fail()
		}
		final := i == len(parts)-1
		if !localDirectorySafe(entry, !final, final && private) {
			return fail()
		}
		file, err := localOpenAt(parent.file, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return fail()
		}
		childRoot, err := parent.root.OpenRoot(name)
		if err != nil {
			_ = file.Close()
			return fail()
		}
		opened, fdErr := file.Stat()
		rooted, rootErr := childRoot.Stat(".")
		if fdErr != nil || rootErr != nil || !os.SameFile(entry, opened) || !os.SameFile(opened, rooted) {
			_ = file.Close()
			_ = childRoot.Close()
			return fail()
		}
		authority.names = append(authority.names, name)
		authority.directories = append(authority.directories, &localDirectory{root: childRoot, file: file, info: opened})
	}
	if !authority.same() {
		return fail()
	}
	return authority, nil
}

func (a *localAuthority) last() *localDirectory { return a.directories[len(a.directories)-1] }
func (a *localAuthority) same() bool {
	for i, d := range a.directories {
		info, err := d.file.Stat()
		final := i == len(a.directories)-1
		if err != nil || !localDirectorySafe(info, !final, final && a.private) || !os.SameFile(d.info, info) {
			return false
		}
		if i > 0 {
			current, err := a.directories[i-1].root.Lstat(a.names[i-1])
			if err != nil || !current.IsDir() || !os.SameFile(current, info) {
				return false
			}
		}
	}
	return true
}
func (a *localAuthority) close() {
	if a == nil {
		return
	}
	for i := len(a.directories) - 1; i >= 0; i-- {
		a.directories[i].close()
	}
}

type localIdentity struct {
	Device uint64
	Inode  uint64
}

type localFingerprint struct {
	Identity  localIdentity
	Size      int64
	MtimeSec  int64
	MtimeNsec int64
	CtimeSec  int64
	CtimeNsec int64
}

func localIdentityOf(info os.FileInfo) localIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return localIdentity{}
	}
	return localIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}
}
func localFingerprintOf(info os.FileInfo) localFingerprint {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return localFingerprint{}
	}
	return localFingerprint{Identity: localIdentityOf(info), Size: info.Size(), MtimeSec: stat.Mtim.Sec, MtimeNsec: stat.Mtim.Nsec, CtimeSec: stat.Ctim.Sec, CtimeNsec: stat.Ctim.Nsec}
}

// Remove only an exclusively created, still-identical regular entry. Never
// recursively clean a subtree or remove a preexisting/ambiguous destination.
func localRemoveOwned(parent *localDirectory, name string, owned os.FileInfo) {
	if owned == nil {
		return
	}
	current, err := parent.root.Lstat(name)
	if err == nil && current.Mode().IsRegular() && os.SameFile(current, owned) {
		_ = parent.root.Remove(name)
	}
}

func localPublish(parent *localDirectory, temp, final string) error {
	if err := unix.Renameat2(int(parent.file.Fd()), temp, int(parent.file.Fd()), final, unix.RENAME_NOREPLACE); err != nil {
		return ErrProvider
	}
	return nil
}
