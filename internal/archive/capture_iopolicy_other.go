//go:build !darwin

package archive

func withCaptureIOPolicy(fn func()) { fn() }
