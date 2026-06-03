// Package ad provides LDAP/Active Directory helpers for use by long-running
// processes (the dash HTTP server). Unlike the CLI shims in ad.go, every
// function here returns an error instead of calling os.Exit.
package ad

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	ldap "github.com/go-ldap/ldap/v3"
	"jot/core/config"
)

// Connect dials the AD server and binds with the service account from cfg.
// The caller must call Close() on the returned connection.
func Connect(cfg config.Jot) (*ldap.Conn, error) {
	var (
		l   *ldap.Conn
		err error
	)
	if strings.HasPrefix(cfg.AD.Server, "ldaps://") {
		l, err = ldap.DialURL(cfg.AD.Server, ldap.DialWithTLSConfig(
			&tls.Config{InsecureSkipVerify: true},
		))
	} else {
		l, err = ldap.DialURL(cfg.AD.Server)
	}
	if err != nil {
		return nil, fmt.Errorf("LDAP dial: %w", err)
	}
	if err := l.Bind(cfg.AD.BindDN, cfg.AD.BindPassword); err != nil {
		l.Close()
		return nil, fmt.Errorf("LDAP bind: %w", err)
	}
	return l, nil
}

// FindUserDN returns the distinguished name for a sAMAccountName.
func FindUserDN(l *ldap.Conn, cfg config.Jot, username string) (string, error) {
	req := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		fmt.Sprintf("(&(objectCategory=person)(objectClass=user)(sAMAccountName=%s))",
			ldap.EscapeFilter(username)),
		[]string{"distinguishedName"}, nil,
	)
	res, err := l.Search(req)
	if err != nil {
		return "", err
	}
	if len(res.Entries) == 0 {
		return "", fmt.Errorf("user not found: %s", username)
	}
	return res.Entries[0].DN, nil
}

// FindGroupDN returns the distinguished name for a group CN.
func FindGroupDN(l *ldap.Conn, cfg config.Jot, cn string) (string, error) {
	req := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		fmt.Sprintf("(&(objectClass=group)(cn=%s))", ldap.EscapeFilter(cn)),
		[]string{"distinguishedName"}, nil,
	)
	res, err := l.Search(req)
	if err != nil {
		return "", err
	}
	if len(res.Entries) == 0 {
		return "", fmt.Errorf("group not found: %s", cn)
	}
	return res.Entries[0].DN, nil
}

// DisableUser sets the ACCOUNTDISABLE bit (0x0002) in userAccountControl
// while preserving all other flags.
func DisableUser(l *ldap.Conn, userDN string) error {
	req := ldap.NewSearchRequest(
		userDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"userAccountControl"}, nil,
	)
	res, err := l.Search(req)
	if err != nil {
		return err
	}
	if len(res.Entries) == 0 {
		return fmt.Errorf("DN not found: %s", userDN)
	}
	current, _ := strconv.Atoi(res.Entries[0].GetAttributeValue("userAccountControl"))
	mod := ldap.NewModifyRequest(userDN, nil)
	mod.Replace("userAccountControl", []string{strconv.Itoa(current | 2)})
	return l.Modify(mod)
}

// Unlock clears the lockoutTime attribute, re-enabling a locked account.
func Unlock(l *ldap.Conn, userDN string) error {
	mod := ldap.NewModifyRequest(userDN, nil)
	mod.Replace("lockoutTime", []string{"0"})
	return l.Modify(mod)
}

// ResetPasswordFlag forces a password change at next logon by setting
// pwdLastSet to 0. Only works over LDAPS.
func ResetPasswordFlag(l *ldap.Conn, userDN string) error {
	mod := ldap.NewModifyRequest(userDN, nil)
	mod.Replace("pwdLastSet", []string{"0"})
	return l.Modify(mod)
}

// ResolveUsername generates a collision-free sAMAccountName.
// Pattern: first initial + last name; appends numeric suffix if taken.
func ResolveUsername(l *ldap.Conn, cfg config.Jot, first, last string) string {
	base := strings.ToLower(string([]rune(first)[0])) +
		strings.ToLower(strings.ReplaceAll(last, " ", ""))
	for i := 0; ; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s%d", base, i)
		}
		req := ldap.NewSearchRequest(
			cfg.AD.BaseDN,
			ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
			fmt.Sprintf("(sAMAccountName=%s)", ldap.EscapeFilter(candidate)),
			[]string{"sAMAccountName"}, nil,
		)
		res, _ := l.Search(req)
		if res == nil || len(res.Entries) == 0 {
			return candidate
		}
	}
}

// ToWindowsFileTime converts a Go time.Time to a Windows FILETIME integer
// for LDAP lastLogonTimestamp comparisons.
func ToWindowsFileTime(t time.Time) int64 {
	epoch := time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)
	return int64(t.UTC().Sub(epoch).Nanoseconds() / 100)
}

// FromWindowsFileTime converts a Windows FILETIME integer to a Go time.Time.
// Returns zero time for "never" sentinels (0 and max int64).
func FromWindowsFileTime(wft int64) time.Time {
	if wft == 0 || wft == 9223372036854775807 {
		return time.Time{}
	}
	const unixEpochOffset int64 = 116444736000000000
	return time.Unix((wft-unixEpochOffset)/10000000, 0).Local()
}

// EncodeUnicodePwd returns the UTF-16LE byte sequence AD requires for the
// unicodePwd attribute. Only accepted over LDAPS.
func EncodeUnicodePwd(password string) string {
	quoted := `"` + password + `"`
	runes := utf16.Encode([]rune(quoted))
	buf := make([]byte, len(runes)*2)
	for i, r := range runes {
		binary.LittleEndian.PutUint16(buf[i*2:], r)
	}
	return string(buf)
}

// ProvisionResult holds the outcome of a successful Provision call.
type ProvisionResult struct {
	Username string
	UPN      string
	Email    string
	DN       string
}

// Provision creates a new user account in the appropriate OU based on role
// ("staff" or "student"). It adds the user to cfg.AD.DefaultGroups and sets
// the default password if the server URL uses LDAPS. Returns the new account
// details on success.
func Provision(l *ldap.Conn, cfg config.Jot, first, last, role, dept string) (ProvisionResult, error) {
	role = strings.ToLower(role)
	if role != "staff" && role != "student" {
		return ProvisionResult{}, fmt.Errorf("role must be 'staff' or 'student', got %q", role)
	}

	username := ResolveUsername(l, cfg, first, last)
	upn := username + cfg.AD.UPNSuffix
	email := username + cfg.AD.EmailSuffix

	targetOU := cfg.AD.StaffOU
	if role == "student" {
		targetOU = cfg.AD.StudentOU
	}
	dn := fmt.Sprintf("CN=%s,%s,%s", username, targetOU, cfg.AD.BaseDN)

	addReq := ldap.NewAddRequest(dn, nil)
	addReq.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user"})
	addReq.Attribute("cn", []string{username})
	addReq.Attribute("givenName", []string{first})
	addReq.Attribute("sn", []string{last})
	addReq.Attribute("displayName", []string{first + " " + last})
	addReq.Attribute("sAMAccountName", []string{username})
	addReq.Attribute("userPrincipalName", []string{upn})
	addReq.Attribute("mail", []string{email})
	if dept != "" {
		addReq.Attribute("department", []string{dept})
	}

	usingLDAPS := strings.HasPrefix(cfg.AD.Server, "ldaps://")
	if usingLDAPS && cfg.AD.DefaultPassword != "" {
		addReq.Attribute("unicodePwd", []string{EncodeUnicodePwd(cfg.AD.DefaultPassword)})
		addReq.Attribute("userAccountControl", []string{"512"}) // enabled
	} else {
		addReq.Attribute("userAccountControl", []string{"514"}) // disabled until password is set
	}

	if err := l.Add(addReq); err != nil {
		return ProvisionResult{}, fmt.Errorf("LDAP add: %w", err)
	}

	for _, groupCN := range cfg.AD.DefaultGroups {
		groupDN, err := FindGroupDN(l, cfg, groupCN)
		if err != nil {
			continue
		}
		mod := ldap.NewModifyRequest(groupDN, nil)
		mod.Add("member", []string{dn})
		_ = l.Modify(mod)
	}

	return ProvisionResult{
		Username: username,
		UPN:      upn,
		Email:    email,
		DN:       dn,
	}, nil
}
