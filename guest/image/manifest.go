package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

// ManifestName is the manifest's file name in an image directory.
const ManifestName = "image.json"

// ManifestVersion is the current manifest version.
const ManifestVersion = 1

// maxManifestSize bounds a manifest read.
const maxManifestSize = 16 << 10

// maxBootFile bounds the kernel and the initramfs.
const maxBootFile = 256 << 20

// File is one artifact of an image: a plain file name in the image directory, its size
// and its SHA-256.
type File struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is image.json. The build writes it last, after every artifact, so an
// image directory that has one names a complete, matching set: the initramfs knows
// how to mount exactly this system disk. The engine refuses to boot a set whose files
// do not match it (a half-copied update, a kernel from another build).
type Manifest struct {
	Version int  `json:"version"`
	Kernel  File `json:"kernel"`
	Initrd  File `json:"initrd"`
	System  File `json:"system"`
}

// Files lists the artifacts in boot order.
func (m *Manifest) Files() []File { return []File{m.Kernel, m.Initrd, m.System} }

// Validate reports the first field out of range.
func (m *Manifest) Validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("image: manifest version %d, want %d", m.Version, ManifestVersion)
	}
	seen := map[string]bool{}
	for _, c := range []struct {
		what string
		f    File
		max  uint64
	}{{"kernel", m.Kernel, maxBootFile}, {"initrd", m.Initrd, maxBootFile}, {"system", m.System, MaxDiskSize + FooterSize}} {
		if !validName(c.f.Name) {
			return fmt.Errorf("image: %s name %q must be a plain file name", c.what, c.f.Name)
		}
		if seen[strings.ToLower(c.f.Name)] {
			return fmt.Errorf("image: %s name %q is used twice", c.what, c.f.Name)
		}
		seen[strings.ToLower(c.f.Name)] = true
		if c.f.Size == 0 || c.f.Size > c.max {
			return fmt.Errorf("image: %s size %d out of range", c.what, c.f.Size)
		}
		if !validHash(c.f.SHA256) {
			return fmt.Errorf("image: %s sha256 %q must be 64 lowercase hex digits", c.what, c.f.SHA256)
		}
	}
	if !strings.HasSuffix(m.System.Name, ".vhd") {
		return fmt.Errorf("image: system disk %q must be a .vhd", m.System.Name)
	}
	if s := m.System.Size; s <= FooterSize || (s-FooterSize)%DiskAlign != 0 {
		return fmt.Errorf("image: system disk size %d is not whole MiB plus a footer", s)
	}
	return nil
}

// validName accepts 1..64 of [A-Za-z0-9._-], not starting with a dot, so a name can
// never leave the image directory or name a device (no separators, no colon).
func validName(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ParseManifest decodes and validates image.json: one JSON object of the current
// version, known fields only, no trailing data.
func ParseManifest(b []byte) (*Manifest, error) {
	if len(b) > maxManifestSize {
		return nil, errors.New("image: manifest too large")
	}
	if !utf8.Valid(b) {
		return nil, errors.New("image: manifest is not UTF-8")
	}
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("image: manifest: %w", err)
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("image: trailing data after the manifest")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Encode renders m as indented JSON with a trailing newline.
func (m *Manifest) Encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Check reads r to the end and reports whether it is exactly f: the same size and
// SHA-256. It reads at most f.Size+1 bytes.
func (f File) Check(r io.Reader) error {
	if f.Size >= math.MaxInt64 {
		return fmt.Errorf("image: %s: size %d out of range", f.Name, f.Size)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, int64(f.Size)+1)) //nolint:gosec // G115: f.Size < MaxInt64 is checked above
	if err != nil {
		return fmt.Errorf("image: read %s: %w", f.Name, err)
	}
	if uint64(n) != f.Size { //nolint:gosec // G115: io.Copy never returns a negative count
		return fmt.Errorf("image: %s is not the file of this image (size differs from %s)", f.Name, ManifestName)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("image: %s is not the file of this image (sha256 %s, %s says %s)", f.Name, got, ManifestName, f.SHA256)
	}
	return nil
}

// Describe returns the File for content read from r, named name.
func Describe(name string, r io.Reader) (File, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return File{}, err
	}
	return File{Name: name, Size: uint64(n), SHA256: hex.EncodeToString(h.Sum(nil))}, nil //nolint:gosec // G115: io.Copy never returns a negative count
}
