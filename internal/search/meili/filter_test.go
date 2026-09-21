package meili

import (
	"strings"
	"testing"

	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
)

func TestBuildFilter(t *testing.T) {
	filter := BuildFilter(map[string]string{
		"sourceServer": "example.net",
		"owner":        "con0123",
		"ignored":      "value",
	}, map[string]bool{
		"sourceServer": true,
		"owner":        true,
	})
	want := `owner = "con0123" AND sourceServer = "example.net"`
	if filter != want {
		t.Fatalf("filter mismatch:\n got: %s\nwant: %s", filter, want)
	}
}

func TestBuildSort(t *testing.T) {
	allowed := map[string]bool{"createdAt": true, "name": true}

	cases := []struct {
		param   string
		want    string
		wantErr bool
	}{
		{param: "", want: ""},
		{param: "createdAt:desc", want: "createdAt:desc"},
		{param: "createdAt:asc", want: "createdAt:asc"},
		{param: "createdAt", want: "createdAt:desc"},
		{param: "owner:desc", wantErr: true},
		{param: "createdAt:random", wantErr: true},
	}
	for _, tc := range cases {
		sort, err := BuildSort(tc.param, allowed)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("BuildSort(%q): expected error, got %v", tc.param, sort)
			}
			continue
		}
		if err != nil {
			t.Fatalf("BuildSort(%q): unexpected error: %v", tc.param, err)
		}
		got := ""
		if len(sort) > 0 {
			got = sort[0]
		}
		if got != tc.want {
			t.Fatalf("BuildSort(%q) = %q, want %q", tc.param, got, tc.want)
		}
	}
}

func TestBuildFilterEscapesValues(t *testing.T) {
	filter := BuildFilter(map[string]string{
		"owner": `con"with\chars`,
	}, map[string]bool{
		"owner": true,
	})
	want := `owner = "con\"with\\chars"`
	if filter != want {
		t.Fatalf("filter mismatch:\n got: %s\nwant: %s", filter, want)
	}
}

func TestDeleteSpecForTarget(t *testing.T) {
	key := "cckv://con0123/concrnt.world/profiles/main/posts/p1"
	parent := "cckv://con0123/concrnt.world/profiles/main/posts"
	cases := []struct {
		target string
		want   DeleteSpec
	}{
		{target: key, want: DeleteSpec{IDs: []string{normalize.EncodeMeiliID(key)}}},
		{target: parent + "/*", want: DeleteSpec{Ancestor: parent}},
		{target: parent + "*", want: DeleteSpec{IDs: []string{normalize.EncodeMeiliID(parent)}, Ancestor: parent}},
		{target: "ccfs://con0123/concrnt/abc", want: DeleteSpec{}},
	}
	for _, tc := range cases {
		got := DeleteSpecForTarget(tc.target)
		if strings.Join(got.IDs, ",") != strings.Join(tc.want.IDs, ",") || got.Ancestor != tc.want.Ancestor {
			t.Fatalf("DeleteSpecForTarget(%q) = %+v, want %+v", tc.target, got, tc.want)
		}
		if got.IsEmpty() != tc.want.IsEmpty() {
			t.Fatalf("DeleteSpecForTarget(%q).IsEmpty() = %v", tc.target, got.IsEmpty())
		}
	}
}
