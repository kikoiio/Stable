//go:build !linux && !darwin

package workspace

import "os"

func safeReadFlags() int                                   { return 0 }
func validateHardlinks(os.FileInfo) error                  { return ErrUnavailable }
func rootIdentity(os.FileInfo) (RootIdentity, error)       { return RootIdentity{}, ErrUnavailable }
func allocatedBytes(os.FileInfo) (int64, error)            { return 0, ErrUnavailable }
func openBeneath(*os.Root, string, bool) (*os.File, error) { return nil, ErrUnavailable }
func allocationUnit(*os.Root) (int64, error)               { return 0, ErrUnavailable }
