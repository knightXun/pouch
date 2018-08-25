package archive

import (
	"archive/tar"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/alibaba/pouch/pkg/system"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/docker/docker/pkg/pools"
	"compress/gzip"
	"fmt"
	"bufio"
)

type (
	// Archive is a type of io.ReadCloser which has two interfaces Read and Closer.
	Archive io.ReadCloser
	// Reader is a type of io.Reader.
	Reader io.Reader
	// Compression is the state represents if compressed or not.
	Compression int
	// WhiteoutFormat is the format of whiteouts unpacked
	WhiteoutFormat int
	// TarChownOptions wraps the chown options UID and GID.
	TarChownOptions struct {
		UID, GID int
	}
	// TarOptions wraps the tar options.
	TarOptions struct {
		IncludeFiles     []string
		ExcludePatterns  []string
		Compression      Compression
		NoLchown         bool
		UIDMaps          []idtools.IDMap
		GIDMaps          []idtools.IDMap
		ChownOpts        *TarChownOptions
		IncludeSourceDir bool
		// WhiteoutFormat is the expected on disk format for whiteout files.
		// This format will be converted to the standard format on pack
		// and from the standard format on unpack.
		WhiteoutFormat WhiteoutFormat
		// When unpacking, specifies whether overwriting a directory with a
		// non-directory is allowed and vice versa.
		NoOverwriteDirNonDir bool
		// For each include when creating an archive, the included name will be
		// replaced with the matching name from this map.
		RebaseNames map[string]string
	}

	// Archiver allows the reuse of most utility functions of this package
	// with a pluggable Untar function. Also, to facilitate the passing of
	// specific id mappings for untar, an archiver can be created with maps
	// which will then be passed to Untar operations
	Archiver struct {
		Untar   func(io.Reader, string, *TarOptions) error
		UIDMaps []idtools.IDMap
		GIDMaps []idtools.IDMap
	}

	// breakoutError is used to differentiate errors related to breaking out
	// When testing archive breakout in the unit tests, this error is expected
	// in order for the test to pass.
	breakoutError error
)

// Errors used or returned by this file.
var (
	ErrNotDirectory      = errors.New("not a directory")
	ErrDirNotExists      = errors.New("no such directory")
	ErrCannotCopyDir     = errors.New("cannot copy directory")
	ErrInvalidCopySource = errors.New("invalid copy source content")
)

const (
	// AUFSWhiteoutFormat is the default format for whiteouts
	AUFSWhiteoutFormat WhiteoutFormat = iota
	// OverlayWhiteoutFormat formats whiteout according to the overlay
	// standard.
	OverlayWhiteoutFormat
)

type tarAppender struct {
	TarWriter *tar.Writer
	Buffer    *bufio.Writer

	// for hardlink mapping
	SeenFiles map[uint64]string
	UIDMaps   []idtools.IDMap
	GIDMaps   []idtools.IDMap

	// For packing and unpacking whiteout files in the
	// non standard format. The whiteout files defined
	// by the AUFS standard are used as the tar whiteout
	// standard.
	WhiteoutConverter tarWhiteoutConverter
}

type tarWhiteoutConverter interface {
	ConvertWrite(*tar.Header, string, os.FileInfo) error
	ConvertRead(*tar.Header, string) (bool, error)
}

const (
	// Uncompressed represents the uncompressed.
	Uncompressed Compression = iota
	// Bzip2 is bzip2 compression algorithm.
	Bzip2
	// Gzip is gzip compression algorithm.
	Gzip
	// Xz is xz compression algorithm.
	Xz
)

// PreserveTrailingDotOrSeparator returns the given cleaned path (after
// processing using any utility functions from the path or filepath stdlib
// packages) and appends a trailing `/.` or `/` if its corresponding  original
// path (from before being processed by utility functions from the path or
// filepath stdlib packages) ends with a trailing `/.` or `/`. If the cleaned
// path already ends in a `.` path segment, then another is not added. If the
// clean path already ends in a path separator, then another is not added.
func PreserveTrailingDotOrSeparator(cleanedPath, originalPath string) string {
	// Ensure paths are in platform semantics
	cleanedPath = normalizePath(cleanedPath)
	originalPath = normalizePath(originalPath)

	if !specifiesCurrentDir(cleanedPath) && specifiesCurrentDir(originalPath) {
		if !hasTrailingPathSeparator(cleanedPath) {
			// Add a separator if it doesn't already end with one (a cleaned
			// path would only end in a separator if it is the root).
			cleanedPath += string(filepath.Separator)
		}
		cleanedPath += "."
	}

	if !hasTrailingPathSeparator(cleanedPath) && hasTrailingPathSeparator(originalPath) {
		cleanedPath += string(filepath.Separator)
	}

	return cleanedPath
}

// assertsDirectory returns whether the given path is
// asserted to be a directory, i.e., the path ends with
// a trailing '/' or `/.`, assuming a path separator of `/`.
func assertsDirectory(path string) bool {
	return hasTrailingPathSeparator(path) || specifiesCurrentDir(path)
}

// hasTrailingPathSeparator returns whether the given
// path ends with the system's path separator character.
func hasTrailingPathSeparator(path string) bool {
	return len(path) > 0 && os.IsPathSeparator(path[len(path)-1])
}

// specifiesCurrentDir returns whether the given path specifies
// a "current directory", i.e., the last path segment is `.`.
func specifiesCurrentDir(path string) bool {
	return filepath.Base(path) == "."
}

// SplitPathDirEntry splits the given path between its directory name and its
// basename by first cleaning the path but preserves a trailing "." if the
// original path specified the current directory.
func SplitPathDirEntry(path string) (dir, base string) {
	cleanedPath := filepath.Clean(normalizePath(path))

	if specifiesCurrentDir(path) {
		cleanedPath += string(filepath.Separator) + "."
	}

	return filepath.Dir(cleanedPath), filepath.Base(cleanedPath)
}

// CopyInfo holds basic info about the source
// or destination path of a copy operation.
type CopyInfo struct {
	Path       string
	Exists     bool
	IsDir      bool
	RebaseName string
}

// CopyInfoSourcePath stats the given path to create a CopyInfo
// struct representing that resource for the source of an archive copy
// operation. The given path should be an absolute local path. A source path
// has all symlinks evaluated that appear before the last path separator ("/"
// on Unix). As it is to be a copy source, the path must exist.
func CopyInfoSourcePath(path string, followLink bool) (CopyInfo, error) {
	// normalize the file path and then evaluate the symbol link
	// we will use the target file instead of the symbol link if
	// followLink is set
	path = normalizePath(path)

	resolvedPath, rebaseName, err := ResolveHostSourcePath(path, followLink)
	if err != nil {
		return CopyInfo{}, err
	}

	stat, err := os.Lstat(resolvedPath)
	if err != nil {
		return CopyInfo{}, err
	}

	return CopyInfo{
		Path:       resolvedPath,
		Exists:     true,
		IsDir:      stat.IsDir(),
		RebaseName: rebaseName,
	}, nil
}

// CopyInfoDestinationPath stats the given path to create a CopyInfo
// struct representing that resource for the destination of an archive copy
// operation. The given path should be an absolute local path.
func CopyInfoDestinationPath(path string) (info CopyInfo, err error) {
	maxSymlinkIter := 10 // filepath.EvalSymlinks uses 255, but 10 already seems like a lot.
	path = normalizePath(path)
	originalPath := path

	stat, err := os.Lstat(path)

	if err == nil && stat.Mode()&os.ModeSymlink == 0 {
		// The path exists and is not a symlink.
		return CopyInfo{
			Path:   path,
			Exists: true,
			IsDir:  stat.IsDir(),
		}, nil
	}

	// While the path is a symlink.
	for n := 0; err == nil && stat.Mode()&os.ModeSymlink != 0; n++ {
		if n > maxSymlinkIter {
			// Don't follow symlinks more than this arbitrary number of times.
			return CopyInfo{}, errors.New("too many symlinks in " + originalPath)
		}

		// The path is a symbolic link. We need to evaluate it so that the
		// destination of the copy operation is the link target and not the
		// link itself. This is notably different than CopyInfoSourcePath which
		// only evaluates symlinks before the last appearing path separator.
		// Also note that it is okay if the last path element is a broken
		// symlink as the copy operation should create the target.
		var linkTarget string

		linkTarget, err = os.Readlink(path)
		if err != nil {
			return CopyInfo{}, err
		}

		if !filepath.IsAbs(linkTarget) {
			// Join with the parent directory.
			dstParent, _ := SplitPathDirEntry(path)
			linkTarget = filepath.Join(dstParent, linkTarget)
		}

		path = linkTarget
		stat, err = os.Lstat(path)
	}

	if err != nil {
		// It's okay if the destination path doesn't exist. We can still
		// continue the copy operation if the parent directory exists.
		if !os.IsNotExist(err) {
			return CopyInfo{}, err
		}

		// Ensure destination parent dir exists.
		dstParent, _ := SplitPathDirEntry(path)

		parentDirStat, err := os.Lstat(dstParent)
		if err != nil {
			return CopyInfo{}, err
		}
		if !parentDirStat.IsDir() {
			return CopyInfo{}, ErrNotDirectory
		}

		return CopyInfo{Path: path}, nil
	}

	// The path exists after resolving symlinks.
	return CopyInfo{
		Path:   path,
		Exists: true,
		IsDir:  stat.IsDir(),
	}, nil
}

// PrepareArchiveCopy prepares the given srcContent archive, which should
// contain the archived resource described by srcInfo, to the destination
// described by dstInfo. Returns the possibly modified content archive along
// with the path to the destination directory which it should be extracted to.
func PrepareArchiveCopy(srcContent io.Reader, srcInfo, dstInfo CopyInfo) (dstDir string, content io.ReadCloser, err error) {
	// Ensure in platform semantics
	srcInfo.Path = normalizePath(srcInfo.Path)
	dstInfo.Path = normalizePath(dstInfo.Path)

	// Separate the destination path between its directory and base
	// components in case the source archive contents need to be rebased.
	dstDir, dstBase := SplitPathDirEntry(dstInfo.Path)
	_, srcBase := SplitPathDirEntry(srcInfo.Path)

	switch {
	case dstInfo.Exists && dstInfo.IsDir:
		// The destination exists as a directory. No alteration
		// to srcContent is needed as its contents can be
		// simply extracted to the destination directory.
		return dstInfo.Path, ioutil.NopCloser(srcContent), nil
	case dstInfo.Exists && srcInfo.IsDir:
		// The destination exists as some type of file and the source
		// content is a directory. This is an error condition since
		// you cannot copy a directory to an existing file location.
		return "", nil, ErrCannotCopyDir
	case dstInfo.Exists:
		// The destination exists as some type of file and the source content
		// is also a file. The source content entry will have to be renamed to
		// have a basename which matches the destination path's basename.
		if len(srcInfo.RebaseName) != 0 {
			srcBase = srcInfo.RebaseName
		}
		return dstDir, RebaseArchiveEntries(srcContent, srcBase, dstBase), nil
	case srcInfo.IsDir:
		// The destination does not exist and the source content is an archive
		// of a directory. The archive should be extracted to the parent of
		// the destination path instead, and when it is, the directory that is
		// created as a result should take the name of the destination path.
		// The source content entries will have to be renamed to have a
		// basename which matches the destination path's basename.
		if len(srcInfo.RebaseName) != 0 {
			srcBase = srcInfo.RebaseName
		}
		return dstDir, RebaseArchiveEntries(srcContent, srcBase, dstBase), nil
	case assertsDirectory(dstInfo.Path):
		// The destination does not exist and is asserted to be created as a
		// directory, but the source content is not a directory. This is an
		// error condition since you cannot create a directory from a file
		// source.
		return "", nil, ErrDirNotExists
	default:
		// The last remaining case is when the destination does not exist, is
		// not asserted to be a directory, and the source content is not an
		// archive of a directory. It this case, the destination file will need
		// to be created when the archive is extracted and the source content
		// entry will have to be renamed to have a basename which matches the
		// destination path's basename.
		if len(srcInfo.RebaseName) != 0 {
			srcBase = srcInfo.RebaseName
		}
		return dstDir, RebaseArchiveEntries(srcContent, srcBase, dstBase), nil
	}

}

// RebaseArchiveEntries rewrites the given srcContent archive replacing
// an occurrence of oldBase with newBase at the beginning of entry names.
func RebaseArchiveEntries(srcContent io.Reader, oldBase, newBase string) io.ReadCloser {
	if oldBase == string(os.PathSeparator) {
		// If oldBase specifies the root directory, use an empty string as
		// oldBase instead so that newBase doesn't replace the path separator
		// that all paths will start with.
		oldBase = ""
	}

	rebased, w := io.Pipe()

	go func() {
		srcTar := tar.NewReader(srcContent)
		rebasedTar := tar.NewWriter(w)

		for {
			hdr, err := srcTar.Next()
			if err == io.EOF {
				// Signals end of archive.
				rebasedTar.Close()
				w.Close()
				return
			}
			if err != nil {
				w.CloseWithError(err)
				return
			}

			hdr.Name = strings.Replace(hdr.Name, oldBase, newBase, 1)

			if err = rebasedTar.WriteHeader(hdr); err != nil {
				w.CloseWithError(err)
				return
			}

			if _, err = io.Copy(rebasedTar, srcTar); err != nil {
				w.CloseWithError(err)
				return
			}
		}
	}()

	return rebased
}

// TarResourceRebase is like TarResource but renames the first path element of
// items in the resulting tar archive to match the given rebaseName if not "".
func TarResourceRebase(sourcePath, rebaseName string) (content Archive, err error) {
	sourcePath = normalizePath(sourcePath)
	if _, err = os.Lstat(sourcePath); err != nil {
		// Catches the case where the source does not exist or is not a
		// directory if asserted to be a directory, as this also causes an
		// error.
		return
	}

	// Separate the source path between its directory and
	// the entry in that directory which we are archiving.
	sourceDir, sourceBase := SplitPathDirEntry(sourcePath)

	filter := []string{sourceBase}

	logrus.Debugf("copying %q from %q", sourceBase, sourceDir)

	return TarWithOptions(sourceDir, &TarOptions{
		Compression:      Uncompressed,
		IncludeFiles:     filter,
		IncludeSourceDir: true,
		RebaseNames: map[string]string{
			sourceBase: rebaseName,
		},
	})
}

// TarWithOptions creates an archive from the directory at `path`, only including files whose relative
// paths are included in `options.IncludeFiles` (if non-nil) or not in `options.ExcludePatterns`.
func TarWithOptions(srcPath string, options *TarOptions) (io.ReadCloser, error) {

	// Fix the source path to work with long path names. This is a no-op
	// on platforms other than Windows.
	srcPath = fixVolumePathPrefix(srcPath)

	patterns, patDirs, exceptions, err := fileutils.CleanPatterns(options.ExcludePatterns)

	if err != nil {
		return nil, err
	}

	pipeReader, pipeWriter := io.Pipe()

	compressWriter, err := CompressStream(pipeWriter, options.Compression)
	if err != nil {
		return nil, err
	}

	go func() {
		ta := &tarAppender{
			TarWriter:         tar.NewWriter(compressWriter),
			Buffer:            pools.BufioWriter32KPool.Get(nil),
			SeenFiles:         make(map[uint64]string),
			UIDMaps:           options.UIDMaps,
			GIDMaps:           options.GIDMaps,
			WhiteoutConverter: getWhiteoutConverter(options.WhiteoutFormat),
		}

		defer func() {
			// Make sure to check the error on Close.
			if err := ta.TarWriter.Close(); err != nil {
				logrus.Errorf("Can't close tar writer: %s", err)
			}
			if err := compressWriter.Close(); err != nil {
				logrus.Errorf("Can't close compress writer: %s", err)
			}
			if err := pipeWriter.Close(); err != nil {
				logrus.Errorf("Can't close pipe writer: %s", err)
			}
		}()

		// this buffer is needed for the duration of this piped stream
		defer pools.BufioWriter32KPool.Put(ta.Buffer)

		// In general we log errors here but ignore them because
		// during e.g. a diff operation the container can continue
		// mutating the filesystem and we can see transient errors
		// from this

		stat, err := os.Lstat(srcPath)
		if err != nil {
			return
		}

		if !stat.IsDir() {
			// We can't later join a non-dir with any includes because the
			// 'walk' will error if "file/." is stat-ed and "file" is not a
			// directory. So, we must split the source path and use the
			// basename as the include.
			if len(options.IncludeFiles) > 0 {
				logrus.Warn("Tar: Can't archive a file with includes")
			}

			dir, base := SplitPathDirEntry(srcPath)
			srcPath = dir
			options.IncludeFiles = []string{base}
		}

		if len(options.IncludeFiles) == 0 {
			options.IncludeFiles = []string{"."}
		}

		seen := make(map[string]bool)

		for _, include := range options.IncludeFiles {
			rebaseName := options.RebaseNames[include]

			walkRoot := getWalkRoot(srcPath, include)
			filepath.Walk(walkRoot, func(filePath string, f os.FileInfo, err error) error {
				if err != nil {
					logrus.Errorf("Tar: Can't stat file %s to tar: %s", srcPath, err)
					return nil
				}

				relFilePath, err := filepath.Rel(srcPath, filePath)
				if err != nil || (!options.IncludeSourceDir && relFilePath == "." && f.IsDir()) {
					// Error getting relative path OR we are looking
					// at the source directory path. Skip in both situations.
					return nil
				}

				if options.IncludeSourceDir && include == "." && relFilePath != "." {
					relFilePath = strings.Join([]string{".", relFilePath}, string(filepath.Separator))
				}

				skip := false

				// If "include" is an exact match for the current file
				// then even if there's an "excludePatterns" pattern that
				// matches it, don't skip it. IOW, assume an explicit 'include'
				// is asking for that file no matter what - which is true
				// for some files, like .dockerignore and Dockerfile (sometimes)
				if include != relFilePath {
					skip, err = fileutils.OptimizedMatches(relFilePath, patterns, patDirs)
					if err != nil {
						logrus.Errorf("Error matching %s: %v", relFilePath, err)
						return err
					}
				}

				if skip {
					// If we want to skip this file and its a directory
					// then we should first check to see if there's an
					// excludes pattern (eg !dir/file) that starts with this
					// dir. If so then we can't skip this dir.

					// Its not a dir then so we can just return/skip.
					if !f.IsDir() {
						return nil
					}

					// No exceptions (!...) in patterns so just skip dir
					if !exceptions {
						return filepath.SkipDir
					}

					dirSlash := relFilePath + string(filepath.Separator)

					for _, pat := range patterns {
						if pat[0] != '!' {
							continue
						}
						pat = pat[1:] + string(filepath.Separator)
						if strings.HasPrefix(pat, dirSlash) {
							// found a match - so can't skip this dir
							return nil
						}
					}

					// No matching exclusion dir so just skip dir
					return filepath.SkipDir
				}

				if seen[relFilePath] {
					return nil
				}
				seen[relFilePath] = true

				// Rename the base resource.
				if rebaseName != "" {
					var replacement string
					if rebaseName != string(filepath.Separator) {
						// Special case the root directory to replace with an
						// empty string instead so that we don't end up with
						// double slashes in the paths.
						replacement = rebaseName
					}

					relFilePath = strings.Replace(relFilePath, include, replacement, 1)
				}

				if err := ta.addTarFile(filePath, relFilePath); err != nil {
					logrus.Errorf("Can't add file %s to tar: %s", filePath, err)
					// if pipe is broken, stop writing tar stream to it
					if err == io.ErrClosedPipe {
						return err
					}
				}
				return nil
			})
		}
	}()

	return pipeReader, nil
}

// addTarFile adds to the tar archive a file from `path` as `name`
func (ta *tarAppender) addTarFile(path, name string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}

	link := ""
	if fi.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}

	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	hdr.Mode = int64(os.FileMode(hdr.Mode))

	name, err = canonicalTarName(name, fi.IsDir())
	if err != nil {
		return fmt.Errorf("tar: cannot canonicalize path: %v", err)
	}
	hdr.Name = name

	inode, err := setHeaderForSpecialDevice(hdr, ta, name, fi.Sys())
	if err != nil {
		return err
	}

	// if it's not a directory and has more than 1 link,
	// it's hardlinked, so set the type flag accordingly
	if !fi.IsDir() && hasHardlinks(fi) {
		// a link should have a name that it links too
		// and that linked name should be first in the tar archive
		if oldpath, ok := ta.SeenFiles[inode]; ok {
			hdr.Typeflag = tar.TypeLink
			hdr.Linkname = oldpath
			hdr.Size = 0 // This Must be here for the writer math to add up!
		} else {
			ta.SeenFiles[inode] = name
		}
	}

	capability, _ := system.Lgetxattr(path, "security.capability")
	if capability != nil {
		hdr.Xattrs = make(map[string]string)
		hdr.Xattrs["security.capability"] = string(capability)
	}

	//handle re-mapping container ID mappings back to host ID mappings before
	//writing tar headers/files. We skip whiteout files because they were written
	//by the kernel and already have proper ownership relative to the host
	if !strings.HasPrefix(filepath.Base(hdr.Name), WhiteoutPrefix) && (ta.UIDMaps != nil || ta.GIDMaps != nil) {
		uid, gid, err := getFileUIDGID(fi.Sys())
		if err != nil {
			return err
		}
		xUID, err := idtools.ToContainer(uid, ta.UIDMaps)
		if err != nil {
			return err
		}
		xGID, err := idtools.ToContainer(gid, ta.GIDMaps)
		if err != nil {
			return err
		}
		hdr.Uid = xUID
		hdr.Gid = xGID
	}

	if ta.WhiteoutConverter != nil {
		if err := ta.WhiteoutConverter.ConvertWrite(hdr, path, fi); err != nil {
			return err
		}
	}

	if err := ta.TarWriter.WriteHeader(hdr); err != nil {
		return err
	}

	if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
		file, err := os.Open(path)
		if err != nil {
			return err
		}

		ta.Buffer.Reset(ta.TarWriter)
		defer ta.Buffer.Reset(nil)
		_, err = io.Copy(ta.Buffer, file)
		file.Close()
		if err != nil {
			return err
		}
		err = ta.Buffer.Flush()
		if err != nil {
			return err
		}
	}

	return nil
}

// CompressStream compresseses the dest with specified compression algorithm.
func CompressStream(dest io.Writer, compression Compression) (io.WriteCloser, error) {
	p := pools.BufioWriter32KPool
	buf := p.Get(dest)
	switch compression {
	case Uncompressed:
		writeBufWrapper := p.NewWriteCloserWrapper(buf, buf)
		return writeBufWrapper, nil
	case Gzip:
		gzWriter := gzip.NewWriter(dest)
		writeBufWrapper := p.NewWriteCloserWrapper(buf, gzWriter)
		return writeBufWrapper, nil
	case Bzip2, Xz:
		// archive/bzip2 does not support writing, and there is no xz support at all
		// However, this is not a problem as docker only currently generates gzipped tars
		return nil, fmt.Errorf("Unsupported compression format %s", (&compression).Extension())
	default:
		return nil, fmt.Errorf("Unsupported compression format %s", (&compression).Extension())
	}
}
// Extension returns the extension of a file that uses the specified compression algorithm.
func (compression *Compression) Extension() string {
	switch *compression {
	case Uncompressed:
		return "tar"
	case Bzip2:
		return "tar.bz2"
	case Gzip:
		return "tar.gz"
	case Xz:
		return "tar.xz"
	}
	return ""
}
// ResolveHostSourcePath decides real path need to be copied with parameters such as
// whether to follow symbol link or not, if followLink is true, resolvedPath will return
// link target of any symbol link file, else it will only resolve symlink of directory
// but return symbol link file itself without resolving.
func ResolveHostSourcePath(path string, followLink bool) (resolvedPath, rebaseName string, err error) {
	if followLink {
		resolvedPath, err = filepath.EvalSymlinks(path)
		if err != nil {
			return
		}

		resolvedPath, rebaseName = GetRebaseName(path, resolvedPath)
	} else {
		dirPath, basePath := filepath.Split(path)

		// if not follow symbol link, then resolve symbol link of parent dir
		var resolvedDirPath string
		resolvedDirPath, err = filepath.EvalSymlinks(dirPath)
		if err != nil {
			return
		}
		// resolvedDirPath will have been cleaned (no trailing path separators) so
		// we can manually join it with the base path element.
		resolvedPath = resolvedDirPath + string(filepath.Separator) + basePath
		if hasTrailingPathSeparator(path) && filepath.Base(path) != filepath.Base(resolvedPath) {
			rebaseName = filepath.Base(path)
		}
	}
	return resolvedPath, rebaseName, nil
}

// GetRebaseName normalizes and compares path and resolvedPath,
// return completed resolved path and rebased file name
func GetRebaseName(path, resolvedPath string) (string, string) {
	// linkTarget will have been cleaned (no trailing path separators and dot) so
	// we can manually join it with them
	var rebaseName string
	if specifiesCurrentDir(path) && !specifiesCurrentDir(resolvedPath) {
		resolvedPath += string(filepath.Separator) + "."
	}

	if hasTrailingPathSeparator(path) && !hasTrailingPathSeparator(resolvedPath) {
		resolvedPath += string(filepath.Separator)
	}

	if filepath.Base(path) != filepath.Base(resolvedPath) {
		// In the case where the path had a trailing separator and a symlink
		// evaluation has changed the last path component, we will need to
		// rebase the name in the archive that is being copied to match the
		// originally requested name.
		rebaseName = filepath.Base(path)
	}
	return resolvedPath, rebaseName
}


// canonicalTarName provides a platform-independent and consistent posix-style
//path for files and directories to be archived regardless of the platform.
func canonicalTarName(name string, isDir bool) (string, error) {
	name, err := CanonicalTarNameForPath(name)
	if err != nil {
		return "", err
	}

	// suffix with '/' for directories
	if isDir && !strings.HasSuffix(name, "/") {
		name += "/"
	}
	return name, nil
}