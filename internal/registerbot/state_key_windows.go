//go:build windows

package registerbot

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

func protectStateKey(key []byte) ([]byte, error)   { return transformStateKey(key, true) }
func unprotectStateKey(key []byte) ([]byte, error) { return transformStateKey(key, false) }

func transformStateKey(data []byte, protect bool) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("registerbot: empty encryption key")
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var output windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output)
	} else {
		err = windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data))) }()
	return append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...), nil
}
