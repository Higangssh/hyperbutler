//go:build windows

// Package wincert reads CA certificates from the Windows certificate stores
// (REQ-UX-009), read-only: hyperbutler never changes a store.
package wincert

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
)

// maxPerStore bounds the certificates enumerated in one store, so a store with an
// absurd number of entries cannot keep the engine busy.
const maxPerStore = 20000

// store is one system store: a location flag and a store name.
type store struct {
	loc  uint32
	name string
	what string // for messages
}

// The trusted roots. CurrentUser\Root is the logical store the user's own programs
// trust; it includes the machine's roots, but LocalMachine and its Group Policy and
// Enterprise (Active Directory) roots are read as well in case it does not. Duplicates
// are dropped by the CertSet. Intermediate stores (CA) are not read: a certificate there
// is a cache entry, not a trust decision.
var rootStores = []store{
	{windows.CERT_SYSTEM_STORE_CURRENT_USER, "Root", `CurrentUser\Root`},
	{windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, "Root", `LocalMachine\Root`},
	{windows.CERT_SYSTEM_STORE_LOCAL_MACHINE_GROUP_POLICY, "Root", `LocalMachine Group Policy\Root`},
	{windows.CERT_SYSTEM_STORE_LOCAL_MACHINE_ENTERPRISE, "Root", `LocalMachine Enterprise\Root`},
}

// The "Untrusted Certificates" stores: a certificate there is never imported.
var disallowedStores = []store{
	{windows.CERT_SYSTEM_STORE_CURRENT_USER, "Disallowed", `CurrentUser\Disallowed`},
	{windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, "Disallowed", `LocalMachine\Disallowed`},
}

// ImportRoots adds the trusted root certificates of the Windows stores to set (only CA
// certificates within their validity period; see dockerenv.CertSet.Add). A store that
// does not exist is skipped; the first other error is returned after every store was
// tried.
func ImportRoots(set *dockerenv.CertSet) error {
	if set.Disallowed == nil {
		set.Disallowed = make(map[[32]byte]bool)
	}
	var firstErr error
	for _, s := range disallowedStores {
		err := enum(s, func(der []byte) { set.Disallowed[sha256Sum(der)] = true })
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, s := range rootStores {
		err := enum(s, func(der []byte) { set.Add(der, true) })
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// enum calls fn with a copy of the DER of every X.509 certificate in s. Certificates
// larger than dockerenv.MaxCertDER are passed truncated to one byte too many, which
// CertSet counts as invalid, without copying them.
func enum(s store, fn func(der []byte)) error {
	name, err := windows.UTF16PtrFromString(s.name)
	if err != nil {
		return err
	}
	h, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		s.loc|windows.CERT_STORE_READONLY_FLAG|windows.CERT_STORE_OPEN_EXISTING_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("open the %s certificate store: %w", s.what, err)
	}
	defer func() { _ = windows.CertCloseStore(h, 0) }()
	var c *windows.CertContext
	for n := 0; ; n++ {
		if n == maxPerStore {
			if c != nil {
				_ = windows.CertFreeCertificateContext(c)
			}
			return fmt.Errorf("the %s certificate store has more than %d certificates; the rest were not read", s.what, maxPerStore)
		}
		c, err = windows.CertEnumCertificatesInStore(h, c) // frees the previous context
		if err != nil {
			if errors.Is(err, syscall.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return nil
			}
			return fmt.Errorf("read the %s certificate store: %w", s.what, err)
		}
		if c.EncodingType&windows.X509_ASN_ENCODING == 0 || c.EncodedCert == nil || c.Length == 0 {
			continue
		}
		l := min(int(c.Length), dockerenv.MaxCertDER+1)
		der := make([]byte, l)
		copy(der, unsafe.Slice(c.EncodedCert, l))
		fn(der)
	}
}
