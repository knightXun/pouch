package archive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alibaba/pouch/pkg/system"
)

// ChangeType represents the change type.
type ChangeType int

const (
	// ChangeModify represents the modify operation.
	ChangeModify = iota
	// ChangeAdd represents the add operation.
	ChangeAdd
	// ChangeDelete represents the delete operation.
	ChangeDelete
)

func (c ChangeType) String() string {
	switch c {
	case ChangeModify:
		return "C"
	case ChangeAdd:
		return "A"
	case ChangeDelete:
		return "D"
	}
	return ""
}

// Change represents a change, it wraps the change type and path.
// It describes changes of the files in the path respect to the
// parent layers. The change could be modify, add, delete.
// This is used for layer diff.
type Change struct {
	Path string
	Kind ChangeType
}

func (change *Change) String() string {
	return fmt.Sprintf("%s %s", change.Kind, change.Path)
}

// Gnu tar and the go tar writer don't have sub-second mtime
// precision, which is problematic when we apply changes via tar
// files, we handle this by comparing for exact times, *or* same
// second count and either a or b having exactly 0 nanoseconds
func sameFsTime(a, b time.Time) bool {
	return a == b ||
		(a.Unix() == b.Unix() &&
			(a.Nanosecond() == 0 || b.Nanosecond() == 0))
}

func sameFsTimeSpec(a, b syscall.Timespec) bool {
	return a.Sec == b.Sec &&
		(a.Nsec == b.Nsec || a.Nsec == 0 || b.Nsec == 0)
}

// Changes walks the path rw and determines changes for the files in the path,
// with respect to the parent layers
func Changes(layers []string, rw string) ([]Change, error) {
	return changes(layers, rw, aufsDeletedFile, aufsMetadataSkip)
}

func aufsMetadataSkip(path string) (skip bool, err error) {
	skip, err = filepath.Match(string(os.PathSeparator)+WhiteoutMetaPrefix+"*", path)
	if err != nil {
		skip = true
	}
	return
}

func aufsDeletedFile(root, path string, fi os.FileInfo) (string, error) {
	f := filepath.Base(path)

	// If there is a whiteout, then the file was removed
	if strings.HasPrefix(f, WhiteoutPrefix) {
		originalFile := f[len(WhiteoutPrefix):]
		return filepath.Join(filepath.Dir(path), originalFile), nil
	}

	return "", nil
}

type skipChange func(string) (bool, error)
type deleteChange func(string, string, os.FileInfo) (string, error)

func changes(layers []string, rw string, dc deleteChange, sc skipChange) ([]Change, error) {
	var (
		changes     []Change
		changedDirs = make(map[string]struct{})
	)

	err := filepath.Walk(rw, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Rebase path
		path, err = filepath.Rel(rw, path)
		if err != nil {
			return err
		}

		// As this runs on the daemon side, file paths are OS specific.
		path = filepath.Join(string(os.PathSeparator), path)

		// Skip root
		if path == string(os.PathSeparator) {
			return nil
		}

		if sc != nil {
			if skip, err := sc(path); skip {
				return err
			}
		}

		change := Change{
			Path: path,
		}

		deletedFile, err := dc(rw, path, f)
		if err != nil {
			return err
		}

		// Find out what kind of modification happened
		if deletedFile != "" {
			change.Path = deletedFile
			change.Kind = ChangeDelete
		} else {
			// Otherwise, the file was added
			change.Kind = ChangeAdd

			// ...Unless it already existed in a top layer, in which case, it's a modification
			for _, layer := range layers {
				stat, err := os.Stat(filepath.Join(layer, path))
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				if err == nil {
					// The file existed in the top layer, so that's a modification

					// However, if it's a directory, maybe it wasn't actually modified.
					// If you modify /foo/bar/baz, then /foo will be part of the changed files only because it's the parent of bar
					if stat.IsDir() && f.IsDir() {
						if f.Size() == stat.Size() && f.Mode() == stat.Mode() && sameFsTime(f.ModTime(), stat.ModTime()) {
							// Both directories are the same, don't record the change
							return nil
						}
					}
					change.Kind = ChangeModify
					break
				}
			}
		}

		// If /foo/bar/file.txt is modified, then /foo/bar must be part of the changed files.
		// This block is here to ensure the change is recorded even if the
		// modify time, mode and size of the parent directory in the rw and ro layers are all equal.
		// Check https://github.com/docker/docker/pull/13590 for details.
		if f.IsDir() {
			changedDirs[path] = struct{}{}
		}
		if change.Kind == ChangeAdd || change.Kind == ChangeDelete {
			parent := filepath.Dir(path)
			if _, ok := changedDirs[parent]; !ok && parent != "/" {
				changes = append(changes, Change{Path: parent, Kind: ChangeModify})
				changedDirs[parent] = struct{}{}
			}
		}

		// Record change
		changes = append(changes, change)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return changes, nil
}

// FileInfo describes the information of a file.
type FileInfo struct {
	parent     *FileInfo
	name       string
	stat       *system.StatT
	children   map[string]*FileInfo
	capability []byte
	added      bool
}

// LookUp looks up the file information of a file.
func (info *FileInfo) LookUp(path string) *FileInfo {
	// As this runs on the daemon side, file paths are OS specific.
	parent := info
	if path == string(os.PathSeparator) {
		return info
	}

	pathElements := strings.Split(path, string(os.PathSeparator))
	for _, elem := range pathElements {
		if elem != "" {
			child := parent.children[elem]
			if child == nil {
				return nil
			}
			parent = child
		}
	}
	return parent
}

func (info *FileInfo) path() string {
	if info.parent == nil {
		// As this runs on the daemon side, file paths are OS specific.
		return string(os.PathSeparator)
	}
	return filepath.Join(info.parent.path(), info.name)
}

func (info *FileInfo) addChanges(oldInfo *FileInfo, changes *[]Change) {

	sizeAtEntry := len(*changes)

	if oldInfo == nil {
		// add
		change := Change{
			Path: info.path(),
			Kind: ChangeAdd,
		}
		*changes = append(*changes, change)
		info.added = true
	}

	// We make a copy so we can modify it to detect additions
	// also, we only recurse on the old dir if the new info is a directory
	// otherwise any previous delete/change is considered recursive
	oldChildren := make(map[string]*FileInfo)
	if oldInfo != nil && info.isDir() {
		for k, v := range oldInfo.children {
			oldChildren[k] = v
		}
	}

	for name, newChild := range info.children {
		oldChild, _ := oldChildren[name]
		if oldChild != nil {
			// change?
			oldStat := oldChild.stat
			newStat := newChild.stat
			// Note: We can't compare inode or ctime or blocksize here, because these change
			// when copying a file into a container. However, that is not generally a problem
			// because any content change will change mtime, and any status change should
			// be visible when actually comparing the stat fields. The only time this
			// breaks down is if some code intentionally hides a change by setting
			// back mtime
			if statDifferent(oldStat, newStat) ||
				bytes.Compare(oldChild.capability, newChild.capability) != 0 {
				change := Change{
					Path: newChild.path(),
					Kind: ChangeModify,
				}
				*changes = append(*changes, change)
				newChild.added = true
			}

			// Remove from copy so we can detect deletions
			delete(oldChildren, name)
		}

		newChild.addChanges(oldChild, changes)
	}
	for _, oldChild := range oldChildren {
		// delete
		change := Change{
			Path: oldChild.path(),
			Kind: ChangeDelete,
		}
		*changes = append(*changes, change)
	}

	// If there were changes inside this directory, we need to add it, even if the directory
	// itself wasn't changed. This is needed to properly save and restore filesystem permissions.
	// As this runs on the daemon side, file paths are OS specific.
	if len(*changes) > sizeAtEntry && info.isDir() && !info.added && info.path() != string(os.PathSeparator) {
		change := Change{
			Path: info.path(),
			Kind: ChangeModify,
		}
		// Let's insert the directory entry before the recently added entries located inside this dir
		*changes = append(*changes, change) // just to resize the slice, will be overwritten
		copy((*changes)[sizeAtEntry+1:], (*changes)[sizeAtEntry:])
		(*changes)[sizeAtEntry] = change
	}

}

// Changes add changes to file information.
func (info *FileInfo) Changes(oldInfo *FileInfo) []Change {
	var changes []Change

	info.addChanges(oldInfo, &changes)

	return changes
}

func newRootFileInfo() *FileInfo {
	// As this runs on the daemon side, file paths are OS specific.
	root := &FileInfo{
		name:     string(os.PathSeparator),
		children: make(map[string]*FileInfo),
	}
	return root
}


