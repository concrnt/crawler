package meili

import "testing"

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
