package testsupport

import (
	"testing"
	"time"
)

func TestScale(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 2 * time.Second, false},
		{"1", 2 * time.Second, false},
		{"8", 16 * time.Second, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"1.5", 0, true},
		{"garbage", 0, true},
		{"9223372036854775807", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("DAEMONPILOT_TEST_TIME_SCALE", tc.value)
			defer func() {
				if got := recover() != nil; got != tc.invalid {
					t.Fatalf("panic=%v want %v", got, tc.invalid)
				}
			}()
			if got := Scale(2 * time.Second); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
