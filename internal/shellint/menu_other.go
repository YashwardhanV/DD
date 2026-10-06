//go:build !windows

package shellint

import "errors"

// WindowsRegistry exists only on Windows; elsewhere every call is unsupported.
type WindowsRegistry struct{}

func (WindowsRegistry) SetValue(string, string, string) error { return errors.ErrUnsupported }
func (WindowsRegistry) GetValue(string, string) (string, bool, error) {
	return "", false, errors.ErrUnsupported
}
func (WindowsRegistry) DeleteTree(string) (bool, error) { return false, errors.ErrUnsupported }
