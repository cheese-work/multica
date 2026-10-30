//go:build !linux

package sandbox

import (
	"errors"
	"os"
)

func pin(string) (*os.File, string, error) {
	return nil, "", errors.New("mount pinning needs Linux")
}
