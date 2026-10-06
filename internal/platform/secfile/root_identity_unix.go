//go:build linux || darwin

package secfile

import "os"

func captureRootIdentity(path string) (rootIdentitySnapshot, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return rootIdentitySnapshot{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return rootIdentitySnapshot{}, ErrUnsafePath
	}
	return rootIdentitySnapshot{file: info}, nil
}

func sameRootIdentity(a, b rootIdentitySnapshot) bool {
	return a.file != nil && b.file != nil && os.SameFile(a.file, b.file)
}
