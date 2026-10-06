//go:build windows

package shellint

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// WindowsRegistry implements Registry on the real HKEY_CURRENT_USER.
type WindowsRegistry struct{}

// SetValue implements Registry.
func (WindowsRegistry) SetValue(key, name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

// GetValue implements Registry.
func (WindowsRegistry) GetValue(key, name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

// DeleteTree implements Registry. Windows refuses to delete a key that still
// has sub-keys, so children are deleted first.
func (WindowsRegistry) DeleteTree(key string) (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	subs, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return false, err
	}
	for _, s := range subs {
		if _, err := (WindowsRegistry{}).DeleteTree(key + `\` + s); err != nil {
			return false, err
		}
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, key); err != nil {
		return false, err
	}
	return true, nil
}
