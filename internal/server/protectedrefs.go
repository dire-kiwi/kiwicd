package server

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nektos/act/pkg/workflowpattern"
)

// Only runs from a repository's protected refs get its secrets and the
// global ones: its default branch, and the refs matching its protected_refs
// patterns. A push run's workflow comes from the pushed ref, so without this
// anyone who can push a branch, such as a collaborator or a bot like
// Dependabot, could add a workflow there that prints every secret. Runs from
// other refs get only secrets.KIWICD_TOKEN, which then fetches only their own
// repository (see jobtoken.go).
//
// repository_push runs use the listener's default branch, and so do
// dispatches unless the admin picks another ref, so they keep their secrets.
//
// A pattern is a branch name like push's branches filter (release/*, where
// * doesn't match / and ** does), or a full ref (refs/tags/v*).

// cleanProtectedRefs checks a repository's protected ref patterns.
func cleanProtectedRefs(names []string) ([]string, error) {
	out := []string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || slices.Contains(out, name) {
			continue
		}
		if strings.HasPrefix(name, "!") {
			return nil, fmt.Errorf("protected ref %q: patterns can't be negated", name)
		}
		if strings.HasPrefix(name, "refs/") && !strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/tags/") {
			return nil, fmt.Errorf("protected ref %q: full refs must start with refs/heads/ or refs/tags/", name)
		}
		if _, err := workflowpattern.CompilePattern(name); err != nil {
			return nil, fmt.Errorf("protected ref %q: %v", name, err)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// refProtected reports whether runs from ref get repo's secrets.
func refProtected(repo *Repo, ref string) bool {
	if ref == "refs/heads/"+repo.DefaultBranch {
		return true
	}
	for _, p := range repo.ProtectedRefs {
		if !strings.HasPrefix(p, "refs/") {
			p = "refs/heads/" + p
		}
		pat, err := workflowpattern.CompilePattern(p)
		if err == nil && !pat.Negative && pat.Regex.MatchString(ref) {
			return true
		}
	}
	return false
}

// protectedBranches lists the repository's branches whose runs get secrets,
// for the UI.
func protectedBranches(repo *Repo) []string {
	out := []string{}
	for ref := range repo.Refs {
		if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok && refProtected(repo, ref) {
			out = append(out, branch)
		}
	}
	sort.Strings(out)
	return out
}

// runGetsSecrets reports whether a run's jobs get secrets: its ref was
// protected when the run was created and still is, so that unprotecting a
// ref takes effect for queued and running jobs too.
func (s *Server) runGetsSecrets(run *Run) bool {
	if !run.Protected {
		return false
	}
	repo, err := s.getRepoByID(run.RepoID)
	return err == nil && refProtected(repo, run.Ref)
}
