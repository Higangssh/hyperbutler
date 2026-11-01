// Package assets embeds the brand images the GUI shows (internal/gui), from
// assets/brand, so there is one copy of each in the repository.
package assets

import _ "embed"

// Mascot is brand/mascot-480.png: the butler robot, 480 px wide, transparent.
//
//go:embed brand/mascot-480.png
var Mascot []byte

// Icon is brand/icon-128.png: the app icon.
//
//go:embed brand/icon-128.png
var Icon []byte
