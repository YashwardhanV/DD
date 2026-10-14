//go:build !windows

package volume

import "errors"

// ErrNotUSB means the drive isn't connected through a USB hub.
var ErrNotUSB = errors.New("the drive is not connected by USB")

// USBLinkFor is only implemented on Windows.
func USBLinkFor(root string) (USBLink, error) { return USBLink{}, errors.ErrUnsupported }

// USBPorts is only implemented on Windows.
func USBPorts() ([]PortInfo, error) { return nil, errors.ErrUnsupported }
