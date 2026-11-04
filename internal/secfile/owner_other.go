//go:build !darwin && !linux

package secfile

import "os"

func checkOwnedByUs(string, os.FileInfo) error { return nil }
