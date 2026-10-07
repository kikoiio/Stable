//go:build windows

package secfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsJournaledMove = "journaled-move"

// secureOpen opens a regular file below root while refusing reparse points at
// every component. Windows CreateFile has no openat equivalent, so the root
// handle is kept open and its identity is checked again after the path walk.
func secureOpen(root, rel string) (*os.File, error) {
	parts, err := validateWindowsRelative(rel)
	if err != nil {
		return nil, err
	}
	if err := ensureWindowsParentsNoReparse(root); err != nil {
		return nil, err
	}
	rootHandle, rootInfo, err := openWindowsPath(root, true)
	if err != nil {
		return nil, mapWindowsPathError(err)
	}
	defer windows.CloseHandle(rootHandle)
	if rootInfo.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return nil, ErrUnsafePath
	}
	if rootInfo.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, ErrUnsafePath
	}

	current := filepath.Clean(root)
	for i, part := range parts {
		current = filepath.Join(current, part)
		isLast := i == len(parts)-1
		h, info, openErr := openWindowsPath(current, !isLast)
		if openErr != nil {
			return nil, mapWindowsPathError(openErr)
		}
		if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			windows.CloseHandle(h)
			return nil, ErrUnsafePath
		}
		if !isLast {
			if info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
				windows.CloseHandle(h)
				return nil, ErrUnsafePath
			}
			windows.CloseHandle(h)
			continue
		}
		if info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			windows.CloseHandle(h)
			return nil, ErrUnsafePath
		}
		file := os.NewFile(uintptr(h), current)
		fileInfo, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, statErr
		}
		if !fileInfo.Mode().IsRegular() {
			_ = file.Close()
			return nil, ErrUnsafePath
		}
		// A replacement of root while the path was being opened must not
		// redirect a read into an unrelated project tree.
		currentRootHandle, currentRoot, recheckErr := openWindowsPath(root, true)
		if recheckErr != nil {
			_ = file.Close()
			return nil, mapWindowsPathError(recheckErr)
		}
		_ = windows.CloseHandle(currentRootHandle)
		if currentRoot.volume != rootInfo.volume || currentRoot.fileIndexHigh != rootInfo.fileIndexHigh || currentRoot.fileIndexLow != rootInfo.fileIndexLow {
			_ = file.Close()
			return nil, ErrRootChanged
		}
		return file, nil
	}
	return nil, ErrUnsafePath
}

// openNoFollow opens a path without following its final reparse point. The
// caller decides whether directories are acceptable; this helper only enforces
// the no-reparse boundary.
func openNoFollow(path string) (*os.File, error) {
	h, info, err := openWindowsPath(path, false)
	if err != nil {
		return nil, mapWindowsPathError(err)
	}
	if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, ErrUnsafePath
	}
	return os.NewFile(uintptr(h), path), nil
}

// exchange is kept for the existing secfile API. Windows callers must select
// journaled-move; returning an error here prevents a caller from assuming an
// atomic swap that the platform cannot provide.
func exchange(dirA, dirB string) error {
	if err := sameDevice(dirA, dirB); err != nil {
		return err
	}
	return ErrUnsupported
}

// windowsDirectoryMoveMode identifies the transaction strategy required on
// this target. It is deliberately a string so the coordinator can persist it
// in a journal without importing Windows types.
func windowsDirectoryMoveMode() string { return windowsJournaledMove }

func transactionMode() string { return windowsJournaledMove }

// sameDevice compares volume serial numbers obtained from handles opened with
// FILE_FLAG_OPEN_REPARSE_POINT. Opening through a reparse point is rejected so
// a junction cannot make two paths appear to share a trusted volume.
func sameDevice(pathA, pathB string) error {
	aHandle, a, err := openWindowsPath(pathA, true)
	if err != nil {
		return mapWindowsPathError(err)
	}
	defer windows.CloseHandle(aHandle)
	bHandle, b, err := openWindowsPath(pathB, true)
	if err != nil {
		return mapWindowsPathError(err)
	}
	defer windows.CloseHandle(bHandle)
	if err := ensureWindowsParentsNoReparse(pathA); err != nil {
		return err
	}
	if err := ensureWindowsParentsNoReparse(pathB); err != nil {
		return err
	}
	if a.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || b.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrUnsafePath
	}
	if a.volume != b.volume {
		return ErrDifferentDevice
	}
	return nil
}

// moveDirectory performs one same-volume move for the non-atomic transaction
// coordinator. The coordinator is responsible for persisting its phase before
// and after this call. Destination replacement is explicit and never uses
// MOVEFILE_COPY_ALLOWED, which would permit a cross-volume copy.
func moveDirectory(from, to string, replace bool) error {
	if err := ensureWindowsParentsNoReparse(from); err != nil {
		return err
	}
	if err := ensureWindowsParentsNoReparse(to); err != nil {
		return err
	}
	fromHandle, fromInfo, err := openWindowsPath(from, true)
	if err != nil {
		return mapWindowsPathError(err)
	}
	defer windows.CloseHandle(fromHandle)
	if fromInfo.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || fromInfo.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return ErrUnsafePath
	}

	destinationParent := filepath.Dir(filepath.Clean(to))
	parentHandle, parentInfo, err := openWindowsPath(destinationParent, true)
	if err != nil {
		return mapWindowsPathError(err)
	}
	defer windows.CloseHandle(parentHandle)
	if parentInfo.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || parentInfo.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return ErrUnsafePath
	}
	if fromInfo.volume != parentInfo.volume {
		return ErrDifferentDevice
	}

	if destinationInfo, destinationErr := windowsPathInfo(to, true); destinationErr == nil {
		if destinationInfo.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || destinationInfo.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return ErrUnsafePath
		}
		if !replace {
			return os.ErrExist
		}
	} else if !errors.Is(destinationErr, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(destinationErr, windows.ERROR_PATH_NOT_FOUND) {
		return mapWindowsPathError(destinationErr)
	} else if replace {
		// A replace request must still identify a concrete target at commit
		// time. Treating a missing target as success would hide a race.
		replace = false
	}

	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	fromPtr, err := windows.UTF16PtrFromString(filepath.Clean(from))
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(filepath.Clean(to))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, flags)
}

// windowsRootIdentity returns a stable volume/file identity for a directory.
// It is intentionally separate from rootIdentity in older secfile.go versions
// so the public Root implementation can adopt it without changing this
// platform adapter's build surface.
func windowsRootIdentity(path string) (string, error) {
	if err := ensureWindowsParentsNoReparse(path); err != nil {
		return "", err
	}
	h, info, err := openWindowsPath(path, true)
	if err != nil {
		return "", mapWindowsPathError(err)
	}
	defer windows.CloseHandle(h)
	if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", ErrUnsafePath
	}
	return windowsIdentity(path, info), nil
}

// windowsFileIdentity returns the volume and file index for a handle. It is
// used by manifest/transaction code to detect replacement without relying on
// names or timestamps.
func windowsFileIdentity(file *os.File) (string, error) {
	if file == nil {
		return "", ErrUnsafePath
	}
	info, err := windowsHandleInfoFor(windows.Handle(file.Fd()))
	if err != nil {
		return "", err
	}
	return windowsIdentity(file.Name(), info), nil
}

type windowsHandleInfo struct {
	attributes    uint32
	volume        uint32
	fileIndexHigh uint32
	fileIndexLow  uint32
}

func openWindowsPath(path string, directory bool) (windows.Handle, windowsHandleInfo, error) {
	ptr, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	info, err := windowsHandleInfoFor(h)
	if err != nil {
		windows.CloseHandle(h)
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	return h, info, nil
}

// ensureWindowsParentsNoReparse walks every parent component with
// FILE_FLAG_OPEN_REPARSE_POINT. Opening only the final path component is not
// enough: CreateFile is still allowed to traverse a junction in an ancestor.
func ensureWindowsParentsNoReparse(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	if volume == "" {
		return ErrUnsafePath
	}
	rest := strings.TrimLeft(strings.TrimPrefix(abs, volume), `/\\`)
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) < 2 {
		return nil
	}
	current := volume + string(filepath.Separator)
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		h, info, openErr := openWindowsPath(current, true)
		if openErr != nil {
			return mapWindowsPathError(openErr)
		}
		if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			windows.CloseHandle(h)
			return ErrUnsafePath
		}
		windows.CloseHandle(h)
	}
	return nil
}

func windowsPathInfo(path string, directory bool) (windowsHandleInfo, error) {
	h, info, err := openWindowsPath(path, directory)
	if err != nil {
		return windowsHandleInfo{}, err
	}
	_ = windows.CloseHandle(h)
	return info, nil
}

func windowsHandleInfoFor(h windows.Handle) (windowsHandleInfo, error) {
	var raw windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &raw); err != nil {
		return windowsHandleInfo{}, err
	}
	return windowsHandleInfo{
		attributes:    raw.FileAttributes,
		volume:        raw.VolumeSerialNumber,
		fileIndexHigh: raw.FileIndexHigh,
		fileIndexLow:  raw.FileIndexLow,
	}, nil
}

func windowsIdentity(path string, info windowsHandleInfo) string {
	return fmt.Sprintf("%s|%08x:%08x:%08x", filepath.Clean(path), info.volume, info.fileIndexHigh, info.fileIndexLow)
}

func validateWindowsRelative(rel string) ([]string, error) {
	if rel == "" || strings.IndexByte(rel, 0) >= 0 || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return nil, ErrUnsafePath
	}
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return nil, ErrUnsafePath
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, ':') {
			return nil, ErrUnsafePath
		}
	}
	return parts, nil
}

func mapWindowsPathError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_CANT_ACCESS_FILE) || errors.Is(err, windows.ERROR_REPARSE_TAG_INVALID) {
		return ErrUnsafePath
	}
	return err
}

func rootMkdirAll(root, rel string, perm os.FileMode) error {
	parts, err := validateWindowsRelative(rel)
	if err != nil {
		return err
	}
	rootHandle, info, err := openWindowsRootWrite(root)
	if err != nil {
		return mapWindowsPathError(err)
	}
	if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(rootHandle)
		return ErrUnsafePath
	}
	current := rootHandle
	defer func() { _ = windows.CloseHandle(current) }()
	for _, part := range parts {
		next, nextInfo, openErr := createWindowsRelative(current, part, true, windows.FILE_OPEN_IF)
		if openErr != nil {
			return mapWindowsPathError(openErr)
		}
		if nextInfo.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || nextInfo.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			_ = windows.CloseHandle(next)
			return ErrUnsafePath
		}
		if perm&0077 == 0 {
			if err := setWindowsHandlePrivate(next); err != nil {
				_ = windows.CloseHandle(next)
				return err
			}
		}
		_ = windows.CloseHandle(current)
		current = next
	}
	return nil
}

func rootWriteFileAtomic(root, rel string, data []byte, perm os.FileMode) error {
	parent, base, err := openWindowsParent(root, rel)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".stable-tmp-" + hex.EncodeToString(nonce[:])
	tmpHandle, _, err := createWindowsRelative(parent, tmp, false, windows.FILE_CREATE)
	if err != nil {
		return mapWindowsPathError(err)
	}
	keep := false
	file := os.NewFile(uintptr(tmpHandle), tmp)
	if file == nil {
		_ = windows.CloseHandle(tmpHandle)
		return errors.New("secfile: unable to wrap temporary file handle")
	}
	defer func() {
		if !keep {
			_ = windowsSetDelete(tmpHandle)
		}
		_ = file.Close()
	}()
	if perm&0077 == 0 {
		if err := setWindowsHandlePrivate(tmpHandle); err != nil {
			return err
		}
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := ensureWindowsRegularTarget(parent, base); err != nil {
		return err
	}
	if err := windowsRenameRelative(tmpHandle, parent, base); err != nil {
		return mapWindowsPathError(err)
	}
	keep = true
	return nil
}

func rootRemoveFile(root, rel string) error {
	parent, base, err := openWindowsParent(root, rel)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	target, info, err := createWindowsRelative(parent, base, false, windows.FILE_OPEN)
	if err != nil {
		return mapWindowsPathError(err)
	}
	defer windows.CloseHandle(target)
	if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return ErrUnsafePath
	}
	return windowsSetDelete(target)
}

func openWindowsParent(root, rel string) (windows.Handle, string, error) {
	parts, err := validateWindowsRelative(rel)
	if err != nil {
		return windows.InvalidHandle, "", err
	}
	rootHandle, info, err := openWindowsRootWrite(root)
	if err != nil {
		return windows.InvalidHandle, "", mapWindowsPathError(err)
	}
	if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(rootHandle)
		return windows.InvalidHandle, "", ErrUnsafePath
	}
	current := rootHandle
	for _, part := range parts[:len(parts)-1] {
		next, nextInfo, openErr := createWindowsRelative(current, part, true, windows.FILE_OPEN)
		if openErr != nil {
			_ = windows.CloseHandle(current)
			return windows.InvalidHandle, "", mapWindowsPathError(openErr)
		}
		if nextInfo.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || nextInfo.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			_ = windows.CloseHandle(next)
			_ = windows.CloseHandle(current)
			return windows.InvalidHandle, "", ErrUnsafePath
		}
		_ = windows.CloseHandle(current)
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func createWindowsRelative(parent windows.Handle, name string, directory bool, disposition uint32) (windows.Handle, windowsHandleInfo, error) {
	name16, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	attrs := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    name16,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	if disposition != windows.FILE_OPEN {
		access |= windows.FILE_GENERIC_WRITE | windows.DELETE
	}
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access, attrs, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition, options, 0, 0)
	if err != nil {
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	info, err := windowsHandleInfoFor(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	return handle, info, nil
}

func openWindowsRootWrite(root string) (windows.Handle, windowsHandleInfo, error) {
	if err := ensureWindowsParentsNoReparse(root); err != nil {
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	ptr, err := windows.UTF16PtrFromString(filepath.Clean(root))
	if err != nil {
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return windows.InvalidHandle, windowsHandleInfo{}, mapWindowsPathError(err)
	}
	info, err := windowsHandleInfoFor(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, windowsHandleInfo{}, err
	}
	return h, info, nil
}

func ensureWindowsRegularTarget(parent windows.Handle, name string) error {
	h, info, err := createWindowsRelative(parent, name, false, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) || errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return mapWindowsPathError(err)
	}
	defer windows.CloseHandle(h)
	if info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return ErrUnsafePath
	}
	return nil
}

func windowsRenameRelative(source, parent windows.Handle, name string) error {
	name16, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	fileNameBytes := (len(name16) - 1) * 2
	var layout windowsRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.FileName))+fileNameBytes)
	info := (*windowsRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.ReplaceIfExists = 1
	info.RootDirectory = parent
	info.FileNameLength = uint32(fileNameBytes)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.FileName[0]))[:fileNameBytes/2:fileNameBytes/2], name16)
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(source, &iosb, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
}

func windowsSetDelete(handle windows.Handle) error {
	var info windowsDispositionInformation
	info.DeleteFile = 1
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(handle, &iosb, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), windows.FileDispositionInformation)
}

func setWindowsHandlePrivate(handle windows.Handle) error {
	sid, err := currentTokenSID()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(currentUserSDDL(sid.String()))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

type windowsRenameInformation struct {
	ReplaceIfExists uint8
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

type windowsDispositionInformation struct{ DeleteFile uint8 }
