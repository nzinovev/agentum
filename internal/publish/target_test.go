package publish

import "testing"

func TestParseRemote(test *testing.T) {
	for _, raw := range []string{"https://github.com/owner/repo", "https://github.com/owner/repo.git", "git@github.com:owner/repo.git", "git@github.com:owner/repo"} {
		target, err := ParseRemote(raw)
		if err != nil || target.Host != "github.com" || target.Owner != "owner" || target.Repository != "repo" {
			test.Fatalf("%q => %+v, %v", raw, target, err)
		}
	}
	for _, raw := range []string{"", "/tmp/repo", "https://token@github.com/owner/repo", "https://github.com/owner/repo?secret=value", "https://github.com/owner/repo#fragment", "https://github.com/../repo", "git@github.com:owner/repo/extra", "http://github.com/owner/repo", "https://github.com/owner/%2e%2e", "git@github.com:owner/repo\n"} {
		if _, err := ParseRemote(raw); err == nil {
			test.Errorf("accepted %q", raw)
		}
	}
}

func TestAPIBaseValidation(test *testing.T) {
	for _, raw := range []string{DefaultAPIBase, "https://git.example.com/api/v3"} {
		if err := ValidateAPIBase(raw); err != nil {
			test.Errorf("%q: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://api.github.com", "https://secret@api.github.com", "https://api.github.com?token=secret", "https://api.github.com/../sink"} {
		if ValidateAPIBase(raw) == nil {
			test.Errorf("accepted %q", raw)
		}
	}
}
