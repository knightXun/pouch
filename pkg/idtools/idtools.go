package idtools

import (
	"os"
	"fmt"
	"path/filepath"
)

// IDMap contains a single entry for user namespace range remapping. An array
// of IDMap entries represents the structure that will be provided to the Linux
// kernel for creating a user namespace.
type IDMap struct {
	ContainerID int `json:"container_id"`
	HostID      int `json:"host_id"`
	Size        int `json:"size"`
}

// ToHost takes an id mapping and a remapped ID, and translates the
// ID to the mapped host ID. If no map is provided, then the translation
// assumes a 1-to-1 mapping and returns the passed in id #
func ToHost(contID int, idMap []IDMap) (int, error) {
	if idMap == nil {
		return contID, nil
	}
	for _, m := range idMap {
		if (contID >= m.ContainerID) && (contID <= (m.ContainerID + m.Size - 1)) {
			hostID := m.HostID + (contID - m.ContainerID)
			return hostID, nil
		}
	}
	return -1, fmt.Errorf("Container ID %d cannot be mapped to a host ID", contID)
}

// GetRootUIDGID retrieves the remapped root uid/gid pair from the set of maps.
// If the maps are empty, then the root uid/gid will default to "real" 0/0
func GetRootUIDGID(uidMap, gidMap []IDMap) (int, int, error) {
	var uid, gid int

	if uidMap != nil {
		xUID, err := ToHost(0, uidMap)
		if err != nil {
			return -1, -1, err
		}
		uid = xUID
	}
	if gidMap != nil {
		xGID, err := ToHost(0, gidMap)
		if err != nil {
			return -1, -1, err
		}
		gid = xGID
	}
	return uid, gid, nil
}

// MkdirAllNewAs creates a directory (include any along the path) and then modifies
// ownership ONLY of newly created directories to the requested uid/gid. If the
// directories along the path exist, no change of ownership will be performed
func MkdirAllNewAs(path string, mode os.FileMode, ownerUID, ownerGID int) error {
	return mkdirAs(path, mode, ownerUID, ownerGID, true, false)
}

func mkdirAs(path string, mode os.FileMode, ownerUID, ownerGID int, mkAll, chownExisting bool) error {
	// make an array containing the original path asked for, plus (for mkAll == true)
	// all path components leading up to the complete path that don't exist before we MkdirAll
	// so that we can chown all of them properly at the end.  If chownExisting is false, we won't
	// chown the full directory path if it exists
	var paths []string
	if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
		paths = []string{path}
	} else if err == nil && chownExisting {
		if err := os.Chown(path, ownerUID, ownerGID); err != nil {
			return err
		}
		// short-circuit--we were called with an existing directory and chown was requested
		return nil
	} else if err == nil {
		// nothing to do; directory path fully exists already and chown was NOT requested
		return nil
	}

	if mkAll {
		// walk back to "/" looking for directories which do not exist
		// and add them to the paths array for chown after creation
		dirPath := path
		for {
			dirPath = filepath.Dir(dirPath)
			if dirPath == "/" {
				break
			}
			if _, err := os.Stat(dirPath); err != nil && os.IsNotExist(err) {
				paths = append(paths, dirPath)
			}
		}
		if err := os.MkdirAll(path, mode); err != nil && !os.IsExist(err) {
			return err
		}
	} else {
		if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
			return err
		}
	}
	// even if it existed, we will chown the requested path + any subpaths that
	// didn't exist when we called MkdirAll
	for _, pathComponent := range paths {
		if err := os.Chown(pathComponent, ownerUID, ownerGID); err != nil {
			return err
		}
	}
	return nil
}

// ToContainer takes an id mapping, and uses it to translate a
// host ID to the remapped ID. If no map is provided, then the translation
// assumes a 1-to-1 mapping and returns the passed in id
func ToContainer(hostID int, idMap []IDMap) (int, error) {
	if idMap == nil {
		return hostID, nil
	}
	for _, m := range idMap {
		if (hostID >= m.HostID) && (hostID <= (m.HostID + m.Size - 1)) {
			contID := m.ContainerID + (hostID - m.HostID)
			return contID, nil
		}
	}
	return -1, fmt.Errorf("Host ID %d cannot be mapped to a container ID", hostID)
}