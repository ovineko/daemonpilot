package httpsource

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

func FuzzManifestDecode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip("bounded manifest")
		}
		source, err := New(Config{ManifestURL: "https://example.test/manifest", DownloadURL: "https://example.test/download", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := source.CheckUpdate(t.Context(), "resident")
		var want *daemonpilot.UpdateInfo
		valid := json.Unmarshal(data, &want) == nil && want != nil && want.Version != "" && want.Size >= 0
		if (err == nil) != valid {
			t.Fatalf("manifest acceptance: %q: %v", data, err)
		}
		if valid && want.Version != "resident" && !reflect.DeepEqual(got, want) {
			t.Fatalf("manifest changed: %+v != %+v", got, want)
		}
	})
}

func TestManifestDocumentBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"complete", `{"version":"next"}`, true},
		{"trailing object", `{"version":"next"}{}`, false},
		{"trailing garbage", `{"version":"next"}garbage`, false},
		{"at limit", `{"version":"next"}` + strings.Repeat(" ", (1<<20)-len(`{"version":"next"}`)), true},
		{"over limit", `{"version":"next"}` + strings.Repeat(" ", 1<<20), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := New(Config{ManifestURL: "https://example.test/manifest", DownloadURL: "https://example.test/download", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.CheckUpdate(t.Context(), "current")
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want %v: %v", err == nil, tc.want, err)
			}
		})
	}
}
