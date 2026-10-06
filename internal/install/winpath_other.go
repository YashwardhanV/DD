//go:build !windows

package install

import "errors"

// WindowsUserPath exists only on Windows; elsewhere every call is unsupported.
type WindowsUserPath struct{}

func (WindowsUserPath) Get() (string, error) { return "", errors.ErrUnsupported }
func (WindowsUserPath) Set(string) error     { return errors.ErrUnsupported }

// ScheduleRemoval exists only on Windows.
func ScheduleRemoval(dir string, names []string) error { return errors.ErrUnsupported }
