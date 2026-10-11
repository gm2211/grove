//go:build !darwin && !linux

package sandbox

import (
	"errors"
	"os"
)

func tryLockFile(*os.File) (bool, error) {
	return false, errors.New("sealed VM slots need darwin or linux")
}

func unlockFile(*os.File) {}
