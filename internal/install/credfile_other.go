//go:build !darwin && !linux

package install

import "os"

func checkOwnedByUs(string, os.FileInfo) error { return nil }
