package pgoverlayconnect

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Branch naming shared with pgoverlay-github (the GitHub App webhook service,
// internal/ghook). The service creates every pull-request branch with these
// functions, so an app that knows its repository and its pull request number
// or git ref derives the same name without asking anyone. The rules are pinned
// by testdata/branch_names.json, which the Go and JavaScript (sdk/js-connect)
// test suites both check; change the table and both implementations together.
//
//	pr-number mode:  gh-<key>-pr-<number>
//	git-branch mode: gh-<key>-<sanitized ref>
//
// <key> is RepoKey(repo): six hex characters of the SHA-256 of the lowercased
// "owner/name". It keeps pull request #7 of one repository from sharing (and,
// on close, destroying) the branch of pull request #7 in another repository
// that reaches the same service.
const (
	// GitHubBranchPrefix starts every branch the GitHub App service creates.
	GitHubBranchPrefix = "gh-"
	// MaxBranchNameLen is the engine's branch-name limit
	// (^[a-z0-9][a-z0-9-]{0,40}$).
	MaxBranchNameLen = 41

	repoKeyLen = 6 // hex characters of the repository discriminator
	refHashLen = 6 // hex characters appended to a truncated ref
)

// RepoKey returns the repository discriminator used in GitHub App branch
// names: the first six hex characters of the SHA-256 of repo ("owner/name"),
// lowercased first because GitHub treats repository names case-insensitively.
func RepoKey(repo string) string {
	return shortHash(strings.ToLower(repo), repoKeyLen)
}

// PRBranchName returns the branch the GitHub App service creates for pull
// request number pr of repo ("owner/name") in pr-number naming mode (the
// default): "gh-<key>-pr-<number>".
func PRBranchName(repo string, pr int) string {
	return repoPrefix(repo) + "pr-" + strconv.Itoa(pr)
}

// RefBranchName returns the branch the GitHub App service creates in
// git-branch naming mode for a pull request of repo whose head branch is ref
// (the branch name, e.g. "feat/login"; not "refs/heads/feat/login"):
// "gh-<key>-<sanitized ref>". A sanitized ref longer than the 31 characters
// left after the prefix is cut to 24 and suffixed with six hex characters of
// the ref's SHA-256, so two long refs with a common start stay distinct.
//
// It returns "" when ref has no ASCII letter or digit; the service then names
// the branch by pull request number (PRBranchName). The service also uses
// PRBranchName for pull requests from forks, whose head ref is chosen outside
// the repository.
func RefBranchName(repo, ref string) string {
	s := sanitize(ref, 0)
	if s == "" {
		return ""
	}
	prefix := repoPrefix(repo)
	budget := MaxBranchNameLen - len(prefix)
	if len(s) > budget {
		s = strings.TrimRight(s[:budget-refHashLen-1], "-") + "-" + shortHash(ref, refHashLen)
	}
	return prefix + s
}

// SanitizeRef maps a git ref to a branch-name fragment of at most 41
// characters: ASCII letters are lowercased, ASCII letters and digits are kept,
// every run of other characters becomes one dash, and leading and trailing
// dashes are dropped.
//
// The fragment is not the name the GitHub App service creates: that name
// carries a prefix and a repository key, see RefBranchName.
func SanitizeRef(ref string) string {
	return sanitize(ref, MaxBranchNameLen)
}

// sanitize implements SanitizeRef with a length cap (maxLen <= 0: no cap). A
// cut never leaves a trailing dash. Only ASCII is case-folded, so the Go and
// JavaScript implementations agree byte for byte (their Unicode case mappings
// differ).
func sanitize(ref string, maxLen int) string {
	var b strings.Builder
	dash := false
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			dash = true
			continue
		}
		if dash && b.Len() > 0 {
			b.WriteByte('-')
		}
		dash = false
		b.WriteByte(c)
	}
	s := b.String()
	if maxLen > 0 && len(s) > maxLen {
		s = strings.TrimRight(s[:maxLen], "-")
	}
	return s
}

func repoPrefix(repo string) string {
	return GitHubBranchPrefix + RepoKey(repo) + "-"
}

func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}
