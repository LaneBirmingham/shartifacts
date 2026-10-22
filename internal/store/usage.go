package store

import (
	"io/fs"
	"os"
	"path/filepath"
)

// AppUsage is how much disk an app takes: its versions' trees, where a file hard-linked
// between versions counts once (as it is stored once), and its backend's data.
type AppUsage struct {
	// Versions is how many versions still keep their files.
	Versions      int   `json:"versions"`
	VersionsBytes int64 `json:"versions_bytes"`
	VersionsFiles int   `json:"versions_files"`
	DataBytes     int64 `json:"data_bytes"`
	DataFiles     int   `json:"data_files"`
}

// AppUsage measures an app's directory.
func (s *Store) AppUsage(id string) (AppUsage, error) {
	a, err := s.App(id)
	if err != nil {
		return AppUsage{}, err
	}
	var u AppUsage
	for _, d := range a.Deployments {
		if !d.Pruned {
			u.Versions++
		}
	}
	seen := map[fileID]bool{}
	if u.VersionsBytes, u.VersionsFiles, err = diskUsage(filepath.Join(s.AppDir(id), "v"), seen); err != nil {
		return AppUsage{}, err
	}
	if u.DataBytes, u.DataFiles, err = diskUsage(s.AppDataDir(id), seen); err != nil {
		return AppUsage{}, err
	}
	return u, nil
}

// diskUsage totals the regular files under root, skipping those already in seen (another
// name for a file counted before) and adding the rest. A missing root is empty.
func diskUsage(root string, seen map[fileID]bool) (total int64, files int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // removed while we walked
			}
			return err
		}
		if id, ok := identify(info); ok {
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		total += info.Size()
		files++
		return nil
	})
	return total, files, err
}
