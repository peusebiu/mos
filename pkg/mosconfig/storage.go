package mosconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/apex/log"
	"golang.org/x/sys/unix"
	"stackerbuild.io/stacker/pkg/atomfs"
	"stackerbuild.io/stacker/pkg/mount"
)

type StorageType string

const (
	AtomfsStorageType   StorageType = "atomfs"
	PuzzlefsStorageType StorageType = "puzzlefs"
)

type Storage interface {
	Type() StorageType

	Mount(t *Target, mountpoint string) (func(), error)
	MountWriteable(t *Target, mountpoint string) (func(), error)

	MountedByHash(target *Target) (string, error)
	TearDownTarget(name string) error
	TargetMountdir(t *Target) (string, error)
	SetupTarget(t *Target) error
}

func NewStorage(opts MosOptions) (Storage, error) {
	var s Storage
	var e error
	switch opts.StorageType {
	case AtomfsStorageType:
		s, e = NewAtomfsStorage(opts.RootDir, opts.StorageCache, opts.ScratchWrites)
	case PuzzlefsStorageType:
		return nil, fmt.Errorf("Not yet implemented")
	default:
		return nil, fmt.Errorf("Unknown storage type requested")
	}

	return s, e
}

type AtomfsStorage struct {
	RootDir     string
	zotPath     string
	scratchPath string
}

func NewAtomfsStorage(rootDir, zotPath, scratchPath string) (*AtomfsStorage, error) {
	return &AtomfsStorage{
		RootDir:     rootDir,
		zotPath:     zotPath,
		scratchPath: scratchPath,
	}, nil
}

func (a *AtomfsStorage) Type() StorageType {
	return AtomfsStorageType
}

// The metadata path which we pass to 'stacker/atomfs' is the directory 'atomfs'
// under *our* scratchdir.
func (a *AtomfsStorage) metadataPath() string {
	return filepath.Join(a.scratchPath, "atomfs")
}

func (a *AtomfsStorage) Mount(t *Target, mountpoint string) (func(), error) {
	if err := EnsureDir(mountpoint); err != nil {
		return func() {}, fmt.Errorf("Failed creating mountpoint %q: %w", mountpoint, err)
	}

	opts := atomfs.MountOCIOpts{
		OCIDir:       filepath.Join(a.zotPath, t.Fullname),
		MetadataPath: a.metadataPath(),
		Tag:          t.Version,
		Target:       mountpoint,
	}
	if !UidmapIsHost() {
		opts.AllowMissingVerityData = true
	}

	mol, err := atomfs.BuildMoleculeFromOCI(opts)
	if err != nil {
		return func() {}, fmt.Errorf("Failed building atomfs molecule for %#v: %w", opts, err)
	}

	cleanup := func() {
		err := atomfs.Umount(mountpoint)
		if err != nil {
			log.Warnf("unmounting %s failed: %s", mountpoint, err)
		}
	}
	err = mol.Mount(mountpoint)
	if err != nil {
		return cleanup, fmt.Errorf("Failed mounting molecule %#v: %w", mol, err)
	}
	return cleanup, nil
}

func (a *AtomfsStorage) MountWriteable(t *Target, mountpoint string) (func(), error) {
	ropath, err := os.MkdirTemp(a.scratchPath, fmt.Sprintf("%s-scratch-readonly-", t.Name))
	if err != nil {
		return func() {}, fmt.Errorf("Failed creating readonly mountpoint: %w", err)
	}

	roCleanup, err := a.Mount(t, ropath)
	if err != nil {
		os.Remove(ropath)
		return func() {}, fmt.Errorf("Failed creating readonly mount for %#v: %w", t, err)
	}

	workdir, err := os.MkdirTemp(a.scratchPath, fmt.Sprintf("%s-scratch-workdir-", t.Name))
	if err != nil {
		roCleanup()
		os.Remove(ropath)
		return func() {}, fmt.Errorf("Failed creating workdir: %w", err)
	}

	upperdir, err := os.MkdirTemp(a.scratchPath, fmt.Sprintf("%s-scratch-upperdir-", t.Name))
	if err != nil {
		roCleanup()
		os.Remove(ropath)
		os.RemoveAll(workdir)
		return func() {}, fmt.Errorf("Failed creating upperdir: %w", err)
	}

	overlayArgs := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,userxattr", ropath, upperdir, workdir)
	err = unix.Mount("overlayfs", mountpoint, "overlay", 0, overlayArgs)
	if err != nil {
		roCleanup()
		os.RemoveAll(workdir)
		os.RemoveAll(upperdir)
		os.Remove(ropath)
		return nil, err
		return func() {}, fmt.Errorf("Failed mounting writeable overlay: %w", err)
	}
	cleanup := func() {
		unix.Unmount(mountpoint, 0)
		roCleanup()
		os.RemoveAll(workdir)
		os.RemoveAll(upperdir)
		os.Remove(ropath)
	}

	return cleanup, nil
}

func getHashFromOverlay(mountinfo string, mountPoint string) (string, error) {
	mounts, err := mount.ParseMounts(mountinfo)
	if err != nil {
		return "", err
	}

	for _, m := range mounts {
		if m.Target != mountPoint {
			continue
		}

		dirs, err := m.GetOverlayDirs()
		if err != nil {
			return "", fmt.Errorf("Failed getting overlay dirs for mount %+v: %w", m, err)
		}

		// atomix has traditionally used the first layer as the 'hash'
		// field of everything.
		firstDir := dirs[0]
		hash := filepath.Base(firstDir)
		return hash, nil
	}

	return "", nil
}

func (a *AtomfsStorage) MountedByHash(target *Target) (string, error) {
	switch target.ServiceType {
	case "hostfs":
		return getHashFromOverlay("/proc/self/mountinfo", a.RootDir)
	case "fs-only":
		/* see SetupTargetRuntime() */
		return getHashFromOverlay("/proc/self/mountinfo", filepath.Join(a.RootDir, "mnt/atom", target.Name))
	case "container":
		// container services are lxc containers, which may or may not
		// have their rootfs visible in this mount namespace. let's
		// look at the specific mountinfo for the container just to be
		// sure.
		out, rc := RunCommandWithRc("lxc-info", "-H", "-n", target.Name, "-s")
		if rc != 0 {
			/* if the service didn't previously exist, it's ok for lxc-ls to fail */
			return "", nil
		}
		if strings.TrimSpace(string(out)) != "RUNNING" {
			return "", nil
		}
		out, rc = RunCommandWithRc("lxc-info", "-H", "-n", target.Name, "-p")
		if rc != 0 {
			/* if the service didn't previously exist, it's ok for lxc-ls to fail */
			return "", nil
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return "", fmt.Errorf("couldn't get pid from %s: %w", strings.TrimSpace(string(out)), err)
		}

		return getHashFromOverlay(fmt.Sprintf("/proc/%d/mountinfo", pid), "/")
	default:
		return "", fmt.Errorf("couldn't determine mountpoint for %s (%s)", target.Name, target.ServiceType)
	}
}

func (a *AtomfsStorage) SetupTarget(t *Target) error {
	mp := filepath.Join(a.scratchPath, "roots", t.Name)
	mounted, err := IsMountpoint(mp)
	if err != nil {
		return fmt.Errorf("Failed checking whether %q is mounted: %w", mp, err)
	}
	if mounted {
		err := atomfs.Umount(mp)
		if err != nil {
			return err
		}
	}

	err = EnsureDir(mp)
	if err != nil {
		return fmt.Errorf("Failed creating mountpoint %q: %w", mp, err)
	}

	if t.ServiceType == "container" {
		// For containers, we have to make this writeable to support
		// uid shifting.  We can un-do this if/when we can use id mapped
		// mounts.
		// XXX TODO we should probably, therefore, umount this after
		// every service stop.
		_, err = a.MountWriteable(t, mp)
	} else {
		_, err = a.Mount(t, mp)
	}
	if err != nil {
		return fmt.Errorf("Failed mounting %s:%s to %q: %w", t.Name, t.Version, mp, err)
	}

	return nil
}

// We mount a readonly copy of the fs under $scratch-writes/roots/$target.
// A container service will want to set lxc.rootfs.path = that, while an
// fs-only service will simply want to do an overlay rw mount onto
// /mnt/atom/$target
func (a *AtomfsStorage) TargetMountdir(t *Target) (string, error) {
	return filepath.Join(a.scratchPath, "roots", t.Name), nil
}

func (a *AtomfsStorage) TearDownTarget(name string) error {
	log.Warnf("tearing down %q", name)
	mp := filepath.Join(a.scratchPath, "roots", name)
	mounted, err := IsMountpoint(mp)
	if err != nil {
		return fmt.Errorf("Failed checking whether %q is mounted: %w", mp, err)
	}
	if !mounted {
		return nil
	}

	err = atomfs.Umount(mp)
	if err != nil {
		return fmt.Errorf("atomfs umount of %q failed: %w", mp, err)
	}
	return err
}
