//go:build !windows

package agent

import "errors"

// Notify: pop-up notifications are Windows-only.
func Notify(title, body string) error { return errors.ErrUnsupported }
