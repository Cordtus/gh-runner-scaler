package github

import "testing"

func TestParsePush_TracksDefaultBranch(t *testing.T) {
	payload := []byte(`{
		"ref":"refs/heads/trunk",
		"after":"0123456789abcdef",
		"repository":{
			"full_name":"Acme/repo",
			"default_branch":"trunk"
		}
	}`)

	event, err := parsePush(payload)
	if err != nil {
		t.Fatalf("parsePush returned error: %v", err)
	}
	if event.DefaultBranch != "trunk" {
		t.Fatalf("expected default branch trunk, got %q", event.DefaultBranch)
	}
	if event.Ref != "refs/heads/trunk" {
		t.Fatalf("expected push ref preserved, got %q", event.Ref)
	}
}
