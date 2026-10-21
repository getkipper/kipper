//go:build windows

package opstunnel

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// ownerOnly builds an ACL granting only the current user full control.
// protect applies it with inheritance from the parent disabled.
func ownerOnly() (*windows.ACL, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
}

func protect(path string) error {
	acl, err := ownerOnly()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// secureDir creates dir and gives it an owner-only DACL, so the socket and
// lock inside inherit it. Before changing permissions, it rejects links and
// junctions and checks ownership with ownedByCurrentUser.
func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("checking %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("%s is not a plain directory, so kip will not put the tunnel's socket there", dir)
	}
	if err := ownedByCurrentUser(dir); err != nil {
		return err
	}
	if err := protect(dir); err != nil {
		return fmt.Errorf("limiting %s to the current user: %w", dir, err)
	}
	return nil
}

// SecureFile replaces path's DACL with one granting only the current user
// full control and disables inherited permissions. It returns security API errors.
func SecureFile(path string) error {
	if err := protect(path); err != nil {
		return fmt.Errorf("limiting %s to the current user: %w", path, err)
	}
	return nil
}

// ownedByCurrentUser refuses path unless its owner is the current user, or
// the Administrators group for an elevated process, which owns what such a
// process creates.
func ownedByCurrentUser(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", path, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if owner.Equals(user.User.Sid) {
		return nil
	}
	if admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err == nil && owner.Equals(admins) {
		if member, err := windows.Token(0).IsMember(admins); err == nil && member {
			return nil
		}
	}
	return fmt.Errorf("%s belongs to another account, so kip will not put the tunnel's socket there", path)
}
