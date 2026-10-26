package upload

import (
	"image"
	_ "image/gif" // register decoders for image.DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path/filepath"
	"strings"

	"github.com/Codeinwp/optimole-cli/internal/api"
)

// imageDimensions reads the width and height from a JPEG, PNG or GIF header,
// or returns nil. It reads only the header, never the whole file.
func imageDimensions(r io.ReaderAt, size int64, name string) *api.Dimensions {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".jpe", ".png", ".gif":
	default:
		return nil
	}
	cfg, _, err := image.DecodeConfig(io.NewSectionReader(r, 0, size))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil
	}
	return &api.Dimensions{Width: cfg.Width, Height: cfg.Height}
}
