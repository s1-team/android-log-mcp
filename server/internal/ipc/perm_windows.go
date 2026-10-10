//go:build windows

package ipc

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictDir gives dir a protected DACL: full control for the current user
// and SYSTEM only (no inherited entries), inherited by the socket file.
func restrictDir(dir string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)", sid))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func currentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

// DirSDDL returns dir's DACL as SDDL (tests).
func DirSDDL(dir string) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}

// CurrentUserSID is the current user's SID string (tests).
func CurrentUserSID() (string, error) { return currentUserSID() }

func restrictSocket(string) error { return nil } // inherits the directory's DACL

func unsupported(err error) bool {
	return errors.Is(err, windows.WSAEAFNOSUPPORT) || errors.Is(err, windows.WSAEPROTONOSUPPORT)
}
