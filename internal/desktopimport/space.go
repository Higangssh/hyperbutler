package desktopimport

// CopyBytes is about how much Run would copy into hyperbutler for sel, as the plan saw
// the items: the size of each new image, and of each volume that is not already there
// with the same content. Images that are there (only tags added), conflicts that are
// refused, unsupported items and volumes Run refuses (live without
// Selection.AllowLiveVolumes, no helper image) count nothing. A volume of the same name
// whose content was not compared, or that is replaced, counts whole. unknown is the
// number of counted volumes Docker Desktop gave no size for; they add nothing to bytes.
func (p *Plan) CopyBytes(sel Selection) (bytes int64, unknown int, err error) {
	imgs, vols, _, err := resolve(p, sel)
	if err != nil {
		return 0, 0, err
	}
	for _, ip := range imgs {
		if ip.Status == StatusNew && ip.Size > 0 {
			bytes += ip.Size
		}
	}
	for _, vp := range vols {
		switch {
		case vp.Status == StatusUnsupported || vp.Status == StatusAlready || p.HelperImage == "":
			continue
		case vp.Status == StatusConflict && !sel.OverwriteVolumes:
			continue
		case len(vp.UsedBy) > 0 && !sel.AllowLiveVolumes:
			continue
		case vp.Size < 0:
			unknown++
			continue
		}
		bytes += vp.Size
	}
	return bytes, unknown, nil
}
