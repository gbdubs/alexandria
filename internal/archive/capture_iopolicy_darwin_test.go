//go:build darwin

package archive

import (
	"runtime"
	"testing"
)

func TestCaptureIOPolicyIsScopedToTheCaptureThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before, err := currentIOPolicy()
	if err != nil {
		t.Fatal(err)
	}
	var during int32
	withCaptureIOPolicy(func() { during, err = currentIOPolicy() })
	if err != nil || during != ioPolicyUtility {
		t.Fatalf("policy during capture = %d, %v", during, err)
	}
	if after, _ := currentIOPolicy(); after != before {
		t.Fatalf("policy was not restored: %d, was %d", after, before)
	}
}
