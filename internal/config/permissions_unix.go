//go:build !windows

package config

import "os"

func restrictFilePermissions(path string) error {
	return os.Chmod(path, 0o600)
}

func filePermissionsRestricted(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 == 0, nil
}
