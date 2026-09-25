//go:build darwin

package archive

import "syscall"

// volumeMount reports the mount point and file system type of the volume
// holding path, without shelling out.
func volumeMount(path string) (string, string, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", "", err
	}
	return cString(stat.Mntonname[:]), cString(stat.Fstypename[:]), nil
}

func cString(value []int8) string {
	bytes := make([]byte, 0, len(value))
	for _, character := range value {
		if character == 0 {
			break
		}
		bytes = append(bytes, byte(character))
	}
	return string(bytes)
}
