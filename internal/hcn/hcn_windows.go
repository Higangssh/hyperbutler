//go:build windows

package hcn

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modComputenetwork = windows.NewLazySystemDLL("computenetwork.dll")
	modOle32          = windows.NewLazySystemDLL("ole32.dll")

	procEnumerateNetworks       = modComputenetwork.NewProc("HcnEnumerateNetworks")
	procCreateNetwork           = modComputenetwork.NewProc("HcnCreateNetwork")
	procOpenNetwork             = modComputenetwork.NewProc("HcnOpenNetwork")
	procQueryNetworkProperties  = modComputenetwork.NewProc("HcnQueryNetworkProperties")
	procDeleteNetwork           = modComputenetwork.NewProc("HcnDeleteNetwork")
	procCloseNetwork            = modComputenetwork.NewProc("HcnCloseNetwork")
	procEnumerateEndpoints      = modComputenetwork.NewProc("HcnEnumerateEndpoints")
	procCreateEndpoint          = modComputenetwork.NewProc("HcnCreateEndpoint")
	procOpenEndpoint            = modComputenetwork.NewProc("HcnOpenEndpoint")
	procQueryEndpointProperties = modComputenetwork.NewProc("HcnQueryEndpointProperties")
	procDeleteEndpoint          = modComputenetwork.NewProc("HcnDeleteEndpoint")
	procCloseEndpoint           = modComputenetwork.NewProc("HcnCloseEndpoint")
	procCoTaskMemFree           = modOle32.NewProc("CoTaskMemFree")
)

// Load reports whether computenetwork.dll and its entry points are present.
func Load() error {
	if err := modComputenetwork.Load(); err != nil {
		return err
	}
	return procCreateNetwork.Find()
}

// takeString copies a CoTaskMemAlloc'd PWSTR returned by HCN and frees it.
func takeString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return s
}

// result turns an HRESULT and error record into an error.
func result(op string, hr uintptr, record *uint16) error {
	rec := takeString(record)
	code := uint32(hr) //nolint:gosec // G115: an HRESULT is 32 bits; the rest of the register is not part of it
	if code == 0 {
		return nil
	}
	msg, _, perr := ParseErrorRecord(rec)
	if perr != nil {
		msg = sanitize(rec, 512)
	}
	return &ResultError{Op: op, HResult: code, Message: msg}
}

func u16(s string) (*uint16, error) { return windows.UTF16PtrFromString(s) }

func parseGUID(id string) (windows.GUID, error) {
	n, err := NormalizeGUID(id)
	if err != nil {
		return windows.GUID{}, err
	}
	return windows.GUIDFromString("{" + n + "}")
}

func enumerate(op string, proc *windows.LazyProc, query string) ([]string, error) {
	q, err := u16(query)
	if err != nil {
		return nil, err
	}
	var out, rec *uint16
	hr, _, _ := proc.Call(uintptr(unsafe.Pointer(q)), uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&rec)))
	doc := takeString(out)
	if err := result(op, hr, rec); err != nil {
		return nil, err
	}
	return ParseIDList(doc)
}

// EnumerateNetworks returns the IDs of all networks HNS knows about. query may be "".
func EnumerateNetworks(query string) ([]string, error) {
	if query == "" {
		query = "{}"
	}
	return enumerate("HcnEnumerateNetworks", procEnumerateNetworks, query)
}

// EnumerateEndpoints returns the IDs of all endpoints HNS knows about.
func EnumerateEndpoints(query string) ([]string, error) {
	if query == "" {
		query = "{}"
	}
	return enumerate("HcnEnumerateEndpoints", procEnumerateEndpoints, query)
}

const propertyQuery = `{"SchemaVersion":{"Major":2,"Minor":0},"Flags":0}`

// NetworkProperties returns the raw properties document of network id.
func NetworkProperties(id string) (string, error) {
	g, err := parseGUID(id)
	if err != nil {
		return "", err
	}
	var h uintptr
	var rec *uint16
	hr, _, _ := procOpenNetwork.Call(uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&rec)))
	if err := result("HcnOpenNetwork", hr, rec); err != nil {
		return "", err
	}
	defer procCloseNetwork.Call(h)
	return queryProps("HcnQueryNetworkProperties", procQueryNetworkProperties, h)
}

// EndpointProperties returns the raw properties document of endpoint id.
func EndpointProperties(id string) (string, error) {
	g, err := parseGUID(id)
	if err != nil {
		return "", err
	}
	var h uintptr
	var rec *uint16
	hr, _, _ := procOpenEndpoint.Call(uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&rec)))
	if err := result("HcnOpenEndpoint", hr, rec); err != nil {
		return "", err
	}
	defer procCloseEndpoint.Call(h)
	return queryProps("HcnQueryEndpointProperties", procQueryEndpointProperties, h)
}

func queryProps(op string, proc *windows.LazyProc, h uintptr) (string, error) {
	q, _ := u16(propertyQuery)
	var out, rec *uint16
	hr, _, _ := proc.Call(h, uintptr(unsafe.Pointer(q)), uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&rec)))
	doc := takeString(out)
	if err := result(op, hr, rec); err != nil {
		return "", err
	}
	return doc, nil
}

// NewID returns a fresh GUID for CreateNetwork or CreateEndpoint. Callers record it
// before creating the object, so a crash between the create and the record cannot leave
// an object nobody knows about.
func NewID() (string, error) {
	g, err := windows.GenerateGUID()
	if err != nil {
		return "", err
	}
	return guidString(g), nil
}

// CreateNetwork creates n with the given ID (from NewID). n.Name must pass ValidateName.
func CreateNetwork(id string, n Network) (string, error) {
	if err := ValidateName(n.Name); err != nil {
		return "", err
	}
	g, err := parseGUID(id)
	if err != nil {
		return "", err
	}
	n.ID = ""
	settings, err := json.Marshal(n)
	if err != nil {
		return "", err
	}
	s, err := u16(string(settings))
	if err != nil {
		return "", err
	}
	var h uintptr
	var rec *uint16
	hr, _, _ := procCreateNetwork.Call(uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&rec)))
	if err := result("HcnCreateNetwork", hr, rec); err != nil {
		return "", err
	}
	procCloseNetwork.Call(h)
	return guidString(g), nil
}

// CreateEndpoint creates e with the given ID (from NewID) on network networkID.
func CreateEndpoint(id, networkID string, e Endpoint) (string, error) {
	if err := ValidateName(e.Name); err != nil {
		return "", err
	}
	ng, err := parseGUID(networkID)
	if err != nil {
		return "", err
	}
	var nh uintptr
	var rec *uint16
	hr, _, _ := procOpenNetwork.Call(uintptr(unsafe.Pointer(&ng)), uintptr(unsafe.Pointer(&nh)), uintptr(unsafe.Pointer(&rec)))
	if err := result("HcnOpenNetwork", hr, rec); err != nil {
		return "", err
	}
	defer procCloseNetwork.Call(nh)

	g, err := parseGUID(id)
	if err != nil {
		return "", err
	}
	e.ID = ""
	e.HostComputeNetwork = guidString(ng)
	settings, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	s, err := u16(string(settings))
	if err != nil {
		return "", err
	}
	var h uintptr
	rec = nil
	hr, _, _ = procCreateEndpoint.Call(nh, uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&rec)))
	if err := result("HcnCreateEndpoint", hr, rec); err != nil {
		return "", err
	}
	procCloseEndpoint.Call(h)
	return guidString(g), nil
}

// DeleteNetwork deletes network id, but only after reading it back and checking that
// its name carries NamePrefix.
func DeleteNetwork(id string) error {
	doc, err := NetworkProperties(id)
	if err != nil {
		return err
	}
	n, err := ParseNetwork(doc)
	if err != nil {
		return err
	}
	if err := ValidateName(n.Name); err != nil {
		return fmt.Errorf("hcn: refusing to delete network %s: %w", id, err)
	}
	g, _ := parseGUID(id)
	var rec *uint16
	hr, _, _ := procDeleteNetwork.Call(uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(&rec)))
	return result("HcnDeleteNetwork", hr, rec)
}

// DeleteEndpoint deletes endpoint id, but only if its name carries NamePrefix.
func DeleteEndpoint(id string) error {
	doc, err := EndpointProperties(id)
	if err != nil {
		return err
	}
	e, err := ParseEndpoint(doc)
	if err != nil {
		return err
	}
	if err := ValidateName(e.Name); err != nil {
		return fmt.Errorf("hcn: refusing to delete endpoint %s: %w", id, err)
	}
	g, _ := parseGUID(id)
	var rec *uint16
	hr, _, _ := procDeleteEndpoint.Call(uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(&rec)))
	return result("HcnDeleteEndpoint", hr, rec)
}

func guidString(g windows.GUID) string {
	s := g.String() // {XXXXXXXX-...}
	return s[1 : len(s)-1]
}
