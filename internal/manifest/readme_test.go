package manifest

import (
	"strings"
	"testing"
)

func TestRenderREADME(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	m.Volumes[0].Consistent = false
	out := RenderREADME(m)
	for _, want := range []string{"# dv-backup archive", "vm-app-01", "2026-09-17T12:00:00Z", "n8n_n8n_data", "n8n", "50.0 MiB", "9.4 MiB", "**not consistent**", "n8n-n8n-1", "not encrypted"} {
		if !strings.Contains(out, want) {
			t.Errorf("README lacks %q:\n%s", want, out)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{-1: "?", 0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 52428800: "50.0 MiB", 3 << 30: "3.0 GiB"}
	for n, want := range cases {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
