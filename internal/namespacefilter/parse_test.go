package namespacefilter

import (
	"testing"
)

func TestParseNamespaceList(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty", raw: "", want: nil},
		{name: "only commas and spaces", raw: " , ,  ,", want: nil},
		{name: "single", raw: "foo", want: []string{"foo"}},
		{name: "trims and drops empties", raw: " foo , ,bar ", want: []string{"foo", "bar"}},
		{name: "dedupes preserving order", raw: "foo,bar,foo,bar", want: []string{"foo", "bar"}},
		{name: "allows aks-owned (operator scope)", raw: "kube-system", want: []string{"kube-system"}},
		{name: "invalid uppercase", raw: "Foo", wantErr: true},
		{name: "invalid underscore", raw: "foo_bar", wantErr: true},
		{name: "invalid one bad among good", raw: "good,BAD", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseNamespaceList(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("length mismatch: got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("index %d: got %q want %q (full got %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}
