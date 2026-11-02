package engine

import (
	"path/filepath"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/imageset"
)

// ImagePin returns the image.json SHA-256 this build was linked with, and whether it
// has one (imageset.Pin, set at link time). A build without a pin accepts any image
// whose files match its own manifest.
func ImagePin() (sum string, pinned bool) { return imageset.Pin() }

// CheckImageDirPin reports whether this build would boot the image in dir as far as the
// pin goes: the image.json there matches it (nil for a development build). The engine
// checks the rest of the image when it boots.
func CheckImageDirPin(dir string) error {
	pin, _ := imageset.Pin()
	return checkImageDirPin(dir, pin)
}

func checkImageDirPin(dir, pin string) error {
	p := filepath.Join(dir, image.ManifestName)
	b, err := ReadFileBounded(p)
	if err != nil {
		return err
	}
	return imageset.CheckPin(pin, p, b)
}
