"""``parse_github_repo``: the single parser for a user-supplied GitHub repository."""

import unittest

from mainloop.services.github_repo import (
    GithubRepo,
    InvalidGithubRepo,
    parse_github_repo,
)


class ParseGithubRepoTests(unittest.TestCase):
    def test_accepted_forms_give_one_canonical_identity(self):
        for text in (
            "oldsj/mainloop",
            "  oldsj/mainloop  ",
            "oldsj/mainloop.git",
            "oldsj/mainloop/",
            "https://github.com/oldsj/mainloop",
            "https://github.com/oldsj/mainloop.git",
            "https://github.com/oldsj/mainloop/",
            "https://GitHub.com/oldsj/mainloop",
        ):
            with self.subTest(text=text):
                repo = parse_github_repo(text)
                self.assertEqual(repo, GithubRepo(owner="oldsj", name="mainloop"))
                self.assertEqual(repo.full_name, "oldsj/mainloop")
                self.assertEqual(repo.html_url, "https://github.com/oldsj/mainloop")

    def test_case_is_kept(self):
        self.assertEqual(
            parse_github_repo("Foo-Bar/My_Repo.js").full_name, "Foo-Bar/My_Repo.js"
        )

    def test_owner_and_name_charsets_and_lengths(self):
        self.assertEqual(parse_github_repo("a/b").full_name, "a/b")
        self.assertEqual(parse_github_repo("a-b/.github").name, ".github")
        self.assertEqual(parse_github_repo(f"{'a' * 39}/{'n' * 100}").owner, "a" * 39)
        for text in (
            f"{'a' * 40}/x",
            f"x/{'n' * 101}",
            "-a/x",
            "a-/x",
            "a_b/x",
            "a b/x",
            "a/b c",
            "a/b$",
            "a/..",
            "a/.",
            "a/.git",
            "a/b?x",
            "ö/x",
        ):
            with self.subTest(text=text), self.assertRaises(InvalidGithubRepo):
                parse_github_repo(text)

    def test_rejects_extra_or_missing_path_segments(self):
        for text in (
            "",
            "   ",
            "oldsj",
            "oldsj/",
            "/mainloop",
            "/oldsj/mainloop",
            "oldsj//mainloop",
            "oldsj/mainloop/tree/main",
            "https://github.com/oldsj",
            "https://github.com/oldsj/mainloop/pull/1",
            "https://github.com/oldsj/mainloop//",
            "https://github.com/",
        ):
            with self.subTest(text=text), self.assertRaises(InvalidGithubRepo):
                parse_github_repo(text)

    def test_rejects_other_hosts_schemes_and_url_parts(self):
        for text in (
            "http://github.com/oldsj/mainloop",
            "git://github.com/oldsj/mainloop",
            "ssh://git@github.com/oldsj/mainloop",
            "git@github.com:oldsj/mainloop.git",
            "https://gitlab.com/oldsj/mainloop",
            "https://www.github.com/oldsj/mainloop",
            "https://github.com.evil.example/oldsj/mainloop",
            "https://evil.example/github.com/oldsj/mainloop",
            "https://user@github.com/oldsj/mainloop",
            "https://user:pass@github.com/oldsj/mainloop",
            "https://github.com:8443/oldsj/mainloop",
            "https://github.com/oldsj/mainloop?x=1",
            "https://github.com/oldsj/mainloop#readme",
            "https://[::1/oldsj/mainloop",
            "github.com/oldsj/mainloop",
        ):
            with self.subTest(text=text), self.assertRaises(InvalidGithubRepo):
                parse_github_repo(text)
