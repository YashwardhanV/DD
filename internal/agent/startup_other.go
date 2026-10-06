//go:build !windows

package agent

import "errors"

// WindowsRegistry exists only on Windows; elsewhere every call is unsupported.
type WindowsRegistry struct{}

func (WindowsRegistry) Get(string) (string, bool, error) { return "", false, errors.ErrUnsupported }
func (WindowsRegistry) Set(string, string) error         { return errors.ErrUnsupported }
func (WindowsRegistry) Delete(string) (bool, error)      { return false, errors.ErrUnsupported }

// WindowsTasks exists only on Windows; elsewhere every call is unsupported.
type WindowsTasks struct{}

func (WindowsTasks) Create(string, string) error { return errors.ErrUnsupported }
func (WindowsTasks) Exists(string) (bool, error) { return false, errors.ErrUnsupported }
func (WindowsTasks) Delete(string) (bool, error) { return false, errors.ErrUnsupported }
