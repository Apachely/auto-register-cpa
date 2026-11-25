//go:build !windows

package registerbot

// Non-Windows installations protect the local key with owner-only file access.
func protectStateKey(key []byte) ([]byte, error)   { return append([]byte(nil), key...), nil }
func unprotectStateKey(key []byte) ([]byte, error) { return append([]byte(nil), key...), nil }
