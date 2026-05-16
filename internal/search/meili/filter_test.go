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
