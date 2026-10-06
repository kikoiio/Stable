//go:build windows

package secfile

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func mkdirAllPrivate(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	return chmodPrivate(path, perm)
}

func openFilePrivate(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if err := chmodPrivate(path, perm); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func chmodPrivate(path string, perm os.FileMode) error {
	if perm&0077 == 0 {
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
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil, nil, dacl, nil)
	}
	if perm&0222 == 0 {
		return os.Chmod(path, perm)
	}
	return nil
}

func currentTokenSID() (*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func ownedByCurrentUser(info os.FileInfo) (bool, error) {
	if info == nil {
		return false, errors.New("secfile: nil file info")
	}
	path := info.Name()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	current, err := currentTokenSID()
	if err != nil {
		return false, err
	}
	return owner.Equals(current), nil
}

func isPrivate(info os.FileInfo) (bool, error) {
	if info == nil {
		return false, errors.New("secfile: nil file info")
	}
	return isPrivatePath(info.Name())
}

func isPrivatePath(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	current, err := currentTokenSID()
	if err != nil {
		return false, err
	}
	if !owner.Equals(current) {
		return false, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil || dacl.AceCount == 0 {
		return false, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			return false, errors.New("secfile: unable to inspect file ACL")
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			return false, nil
		}
		sidPtr := (*windows.SID)(unsafe.Pointer(uintptr(unsafe.Pointer(ace)) + unsafe.Offsetof(ace.SidStart)))
		if !sidPtr.Equals(current) {
			return false, nil
		}
	}
	return true, nil
}
