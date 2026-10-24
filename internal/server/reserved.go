package server

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Reserved labels keep runners for particular repositories and refs, such as
// a deploy runner that holds credentials or can reach production. A runner
// with a reserved label only takes jobs of runs those repositories and refs
// allow, whatever labels the job asks for (self-hosted included), and a job
// that asks for the label from anywhere else fails at once.
type reservedLabels map[string][]labelGrant // by lower-case label

type labelGrant struct {
	repo string // owner/name, lower case
	ref  string // a path.Match pattern over the full ref; "" for the default branch
}

func (g labelGrant) String() string {
	if g.ref == "" {
		return g.repo
	}
	return g.repo + "@" + g.ref
}

// parseReservedLabels parses specs of the form LABEL=REPO[@REF][,REPO[@REF]...].
// A bare REPO allows only its default branch. REF is a branch name or a
// full ref (refs/tags/v*), and may be a pattern: REPO@* allows any branch.
func parseReservedLabels(specs []string) (reservedLabels, error) {
	out := reservedLabels{}
	for _, spec := range specs {
		label, grants, ok := strings.Cut(strings.TrimSpace(spec), "=")
		label = strings.ToLower(strings.TrimSpace(label))
		if !ok || label == "" || strings.TrimSpace(grants) == "" {
			return nil, fmt.Errorf("reserved label %q: expected LABEL=OWNER/REPO[@REF][,...]", spec)
		}
		for _, g := range strings.Split(grants, ",") {
			repo, ref, hasRef := strings.Cut(strings.TrimSpace(g), "@")
			if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				return nil, fmt.Errorf("reserved label %q: %q is not OWNER/REPO", label, repo)
			}
			grant := labelGrant{repo: strings.ToLower(repo)}
			if hasRef {
				if ref == "" {
					return nil, fmt.Errorf("reserved label %q: empty ref after %s@", label, repo)
				}
				if !strings.HasPrefix(ref, "refs/") {
					ref = "refs/heads/" + ref
				}
				if _, err := path.Match(ref, ""); err != nil {
					return nil, fmt.Errorf("reserved label %q: bad pattern %q", label, ref)
				}
				grant.ref = ref
			}
			out[label] = append(out[label], grant)
		}
	}
	return out, nil
}

// allows reports whether a run of repo at ref may use the label.
func (rl reservedLabels) allows(label, repo, ref, defaultBranch string) bool {
	grants, reserved := rl[strings.ToLower(label)]
	if !reserved {
		return true
	}
	for _, g := range grants {
		if g.repo != strings.ToLower(repo) {
			continue
		}
		if g.ref == "" {
			if ref == "refs/heads/"+defaultBranch {
				return true
			}
		} else if ok, _ := path.Match(g.ref, ref); ok {
			return true
		}
	}
	return false
}

func (rl reservedLabels) reservesAny(labels []string) bool {
	for _, l := range labels {
		if _, ok := rl[strings.ToLower(l)]; ok {
			return true
		}
	}
	return false
}

// denied returns the first of labels that a run of repo at ref may not use.
func (rl reservedLabels) denied(labels []string, repo, ref, defaultBranch string) (string, bool) {
	for _, l := range labels {
		if !rl.allows(l, repo, ref, defaultBranch) {
			return l, true
		}
	}
	return "", false
}

// describe lists who may use those of labels that are reserved, for the
// Runners page.
func (rl reservedLabels) describe(labels []string) map[string][]string {
	var out map[string][]string
	for _, l := range labels {
		grants, ok := rl[strings.ToLower(l)]
		if !ok {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		for _, g := range grants {
			out[l] = append(out[l], g.String())
		}
		sort.Strings(out[l])
	}
	return out
}

// runDenied returns a label of labels that the run may not use. The run's
// repository is looked up only when some label is reserved.
func (s *Server) runDenied(run *Run, labels []string) (string, bool) {
	if len(s.reserved) == 0 {
		return "", false
	}
	defaultBranch := ""
	if repo, err := s.getRepoByID(run.RepoID); err == nil {
		defaultBranch = repo.DefaultBranch
	}
	return s.reserved.denied(labels, run.Repo, run.Ref, defaultBranch)
}
