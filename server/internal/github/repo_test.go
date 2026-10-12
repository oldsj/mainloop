package github_test

import (
	"testing"

	"github.com/oldsj/mainloop/server/internal/github"
)

// Mined from backend/tests/test_github_repo.py.
func TestParseRepoAcceptedFormsGiveOneIdentity(t *testing.T) {
	for _, text := range []string{
		"oldsj/mainloop",
		"  oldsj/mainloop  ",
		"oldsj/mainloop.git",
		"oldsj/mainloop.GIT",
		"oldsj/mainloop.Git/",
		"oldsj/mainloop/",
		"https://github.com/oldsj/mainloop",
		"https://github.com/oldsj/mainloop.git",
		"https://github.com/oldsj/mainloop/",
		"https://GitHub.com/oldsj/mainloop",
	} {
		repo, err := github.ParseRepo(text)
		if err != nil {
			t.Errorf("%q: %v", text, err)
			continue
		}
		if repo != (github.Repo{Owner: "oldsj", Name: "mainloop"}) || repo.FullName() != "oldsj/mainloop" ||
			repo.HTMLURL() != "https://github.com/oldsj/mainloop" {
			t.Errorf("%q: got %+v", text, repo)
		}
	}
}

func TestParseRepoKeepsCase(t *testing.T) {
	for in, want := range map[string]string{"Foo-Bar/My_Repo.js": "Foo-Bar/My_Repo.js", "Oldsj/Mainloop.GIT": "Oldsj/Mainloop"} {
		repo, err := github.ParseRepo(in)
		if err != nil || repo.FullName() != want {
			t.Errorf("%q: got %q, %v", in, repo.FullName(), err)
		}
	}
}

func TestParseRepoCharsetsAndLengths(t *testing.T) {
	long := func(c string, n int) string {
		s := ""
		for range n {
			s += c
		}
		return s
	}
	for _, ok := range []string{"a/b", "a-b/.github", long("a", 39) + "/" + long("n", 100)} {
		if _, err := github.ParseRepo(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		long("a", 40) + "/x", "x/" + long("n", 101), "-a/x", "a-/x", "a_b/x", "a b/x", "a/b c", "a/b$",
		"a/..", "a/.", "a/.git", "a/b?x", "ö/x",
	} {
		if _, err := github.ParseRepo(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseRepoRejectsExtraOrMissingSegments(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "oldsj", "oldsj/", "/mainloop", "/oldsj/mainloop", "oldsj//mainloop",
		"oldsj/mainloop/tree/main", "https://github.com/oldsj", "https://github.com/oldsj/mainloop/pull/1",
		"https://github.com/oldsj/mainloop//", "https://github.com/",
	} {
		if _, err := github.ParseRepo(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseRepoRejectsOtherHostsSchemesAndURLParts(t *testing.T) {
	for _, bad := range []string{
		"http://github.com/oldsj/mainloop", "git://github.com/oldsj/mainloop", "ssh://git@github.com/oldsj/mainloop",
		"git@github.com:oldsj/mainloop.git", "https://gitlab.com/oldsj/mainloop", "https://www.github.com/oldsj/mainloop",
		"https://github.com.evil.example/oldsj/mainloop", "https://evil.example/github.com/oldsj/mainloop",
		"https://user@github.com/oldsj/mainloop", "https://user:pass@github.com/oldsj/mainloop",
		"https://github.com:8443/oldsj/mainloop", "https://github.com/oldsj/mainloop?x=1",
		"https://github.com/oldsj/mainloop#readme", "https://[::1/oldsj/mainloop", "github.com/oldsj/mainloop",
	} {
		if _, err := github.ParseRepo(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRepoSameIgnoresCase(t *testing.T) {
	if !(github.Repo{Owner: "Foo", Name: "Bar"}).Same(github.Repo{Owner: "foo", Name: "BAR"}) {
		t.Fatal("same repository with different case not matched")
	}
	if (github.Repo{Owner: "foo", Name: "bar"}).Same(github.Repo{Owner: "foo", Name: "baz"}) {
		t.Fatal("different repositories matched")
	}
}
