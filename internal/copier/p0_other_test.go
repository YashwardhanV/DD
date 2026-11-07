//go:build !windows

package copier

import "unsafe"

func addr(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }
