//go:build windows

package credentials

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

const cryptProtectUIForbidden = 0x1

type platformProtector struct{}

func (platformProtector) Protect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, errors.New("cannot protect an empty credential")
	}
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, cryptProtectUIForbidden, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return copyBlob(out), nil
}

func (platformProtector) Unprotect(protected []byte) ([]byte, error) {
	if len(protected) == 0 {
		return nil, errors.New("cannot unprotect an empty credential")
	}
	in := windows.DataBlob{Size: uint32(len(protected)), Data: &protected[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, cryptProtectUIForbidden, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return copyBlob(out), nil
}

func copyBlob(blob windows.DataBlob) []byte {
	if blob.Size == 0 || blob.Data == nil {
		return nil
	}
	return append([]byte(nil), unsafe.Slice(blob.Data, int(blob.Size))...)
}
