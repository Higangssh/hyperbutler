package dockerapi

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// A container or volume name or id: what dockerd accepts as a name
	// ([a-zA-Z0-9][a-zA-Z0-9_.-]+), or a hex id. It cannot be "." or "..", hold a '/', or
	// start with '-', so it is one plain path segment of the API URL.
	reName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
	// An image reference or id: [registry[:port]/]path[:tag][@digest], or sha256:hex.
	reImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
)

// IsName reports whether s is a container or volume name or id as ValidName accepts it.
func IsName(s string) bool { return reName.MatchString(s) }

// ValidName checks a container or volume name or id before it goes into an API path.
func ValidName(what, s string) error {
	if !reName.MatchString(s) {
		return fmt.Errorf("%s %q is not a valid name or id (letters, digits, '_', '.', '-'; starting with a letter or digit)", what, Clean(s, 80))
	}
	return nil
}

// ValidImageRef checks an image reference or id before it goes into an API path: no
// empty, "." or ".." path segment, so it stays within the API's /images/ path.
func ValidImageRef(s string) error {
	bad := !reImage.MatchString(s)
	if !bad {
		for seg := range strings.SplitSeq(s, "/") {
			if seg == "" || seg == "." || seg == ".." {
				bad = true
				break
			}
		}
	}
	if bad {
		return fmt.Errorf("image %q is not a valid image reference (such as busybox, busybox:1.37 or ghcr.io/owner/name:tag)", Clean(s, 80))
	}
	return nil
}
