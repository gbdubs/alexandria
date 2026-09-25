//go:build !darwin

package archive

import "errors"

func volumeMount(string) (string, string, error) {
	return "", "", errors.New("volume inspection is available only on macOS")
}
