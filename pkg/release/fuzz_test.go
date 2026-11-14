package release

import (
	"net/url"
	"strings"
	"testing"
)

func FuzzReleaseRoute(f *testing.F) {
	f.Fuzz(func(t *testing.T, path string) {
		kind, goos, arch := route(path)
		if kind == "" {
			if goos != "" || arch != "" {
				t.Fatal("partial invalid route")
			}
			return
		}
		if !allowedOS(goos) || !allowedArch(arch) {
			t.Fatal("unknown platform accepted")
		}
		suffix := "/" + goos + "/" + arch
		if !strings.HasSuffix(path, suffix) {
			t.Fatal("route lost platform")
		}
		prefix := strings.TrimSuffix(path, suffix)
		if kind == "manifest" {
			if !strings.HasSuffix(prefix, "manifest") {
				t.Fatal("bad manifest route")
			}
			return
		}
		version := prefix[strings.LastIndex(prefix, "/")+1:]
		decoded, err := url.PathUnescape(version)
		if err != nil || decoded == "" {
			t.Fatal("garbage version accepted")
		}
	})
}
