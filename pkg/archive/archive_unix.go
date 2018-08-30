package archive

import (
	"path/filepath"
	"syscall"
	"errors"
	"archive/tar"
)

// fixVolumePathPrefix does platform specific processing to ensure that if
// the path being passed in is not in a volume path format, convert it to one.
func fixVolumePathPrefix(srcPath string) string {
	return srcPath
}

// getWalkRoot calculates the root path when performing a TarWithOptions.
// We use a separate function as this is platform specific. On Linux, we
// can't use filepath.Join(srcPath,include) because this will clean away
// a trailing "." or "/" which may be important.
func getWalkRoot(srcPath string, include string) string {
	return srcPath + string(filepath.Separator) + include
}

// CanonicalTarNameForPath returns platform-specific filepath
// to canonical posix-style path for tar archival. p is relative
// path.
func CanonicalTarNameForPath(p string) (string, error) {
	return p, nil // already unix-style
}

// chmodTarEntry is used to adjust the file permissions used in tar header based
// on the platform the archival is done.


func getFileUIDGID(stat interface{}) (int, int, error) {
	s, ok := stat.(*syscall.Stat_t)

	if !ok {
		return -1, -1, errors.New("cannot convert stat value to syscall.Stat_t")
	}
	return int(s.Uid), int(s.Gid), nil
}

func major(device uint64) uint64 {
	return (device >> 8) & 0xfff
}

func minor(device uint64) uint64 {
	return (device & 0xff) | ((device >> 12) & 0xfff00)
}

func setHeaderForSpecialDevice(hdr *tar.Header, ta *tarAppender, name string, stat interface{}) (inode uint64, err error) {
	s, ok := stat.(*syscall.Stat_t)

	if !ok {
		err = errors.New("cannot convert stat value to syscall.Stat_t")
		return
	}

	inode = uint64(s.Ino)

	// Currently go does not fill in the major/minors
	if s.Mode&syscall.S_IFBLK != 0 ||
		s.Mode&syscall.S_IFCHR != 0 {
		hdr.Devmajor = int64(major(uint64(s.Rdev)))
		hdr.Devminor = int64(minor(uint64(s.Rdev)))
	}

	return
}



// Mkdev is used to build the value of linux devices (in /dev/) which specifies major
// and minor number of the newly created device special file.
// Linux device nodes are a bit weird due to backwards compat with 16 bit device nodes.
// They are, from low to high: the lower 8 bits of the minor, then 12 bits of the major,
// then the top 12 bits of the minor.
func Mkdev(major int64, minor int64) uint32 {
	return uint32(((minor & 0xfff00) << 12) | ((major & 0xfff) << 8) | (minor & 0xff))
}

// handleTarTypeBlockCharFifo is an OS-specific helper function used by
// createTarFile to handle the following types of header: Block; Char; Fifo
func handleTarTypeBlockCharFifo(hdr *tar.Header, path string) error {
	mode := uint32(hdr.Mode & 07777)
	switch hdr.Typeflag {
	case tar.TypeBlock:
		mode |= syscall.S_IFBLK
	case tar.TypeChar:
		mode |= syscall.S_IFCHR
	case tar.TypeFifo:
		mode |= syscall.S_IFIFO
	}

	if err :=  syscall.Mknod(path, mode, int(Mkdev(hdr.Devmajor, hdr.Devminor))); err != nil {
		return err
	}
	return nil
}