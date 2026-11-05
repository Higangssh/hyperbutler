package copyin

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const tagSurrogate = 0x20000000

// isLink reports whether the reparse point at name is a name surrogate (a junction, a
// symbolic link, a mount point), as opposed to a cloud placeholder or a deduplicated
// file, which stand for their own content. Unknown means a link.
func isLink(name string) bool {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return true
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h) //nolint:errcheck // a query handle
	var info [2]uint32           // FILE_ATTRIBUTE_TAG_INFO
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&info[0])), 8); err != nil {
		return true
	}
	return info[1]&tagSurrogate != 0
}
