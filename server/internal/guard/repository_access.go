package guard

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// CheckRepository checks a GitHub owner/name or clone URL against the workspace
// boundary. Unknown identities fail closed when a boundary is configured.
func (p *Policy) CheckRepository(raw string) error {
	if p.lockdown {
		return fmt.Errorf("%s", lockdownMessage)
	}
	if len(p.RepositoryAllowlist) == 0 {
		return nil
	}
	repo, err := repositoryIdentity(raw)
	if err != nil {
		return err
	}
	owner, name, _ := strings.Cut(repo, "/")
	for restrictedOwner, allowed := range p.RepositoryAllowlist {
		if !strings.EqualFold(owner, restrictedOwner) {
			continue
		}
		for _, candidate := range allowed {
			if strings.EqualFold(name, candidate) {
				return nil
			}
		}
		return fmt.Errorf("%s is not approved by the workspace repository allowlist", repo)
	}
	return nil
}

func repositoryIdentity(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(value), "git@github.com:") {
		value = value[len("git@github.com:"):]
	} else if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("cannot resolve repository identity")
		}
		value = strings.TrimPrefix(u.Path, "/")
	} else if strings.HasPrefix(strings.ToLower(value), "github.com/") {
		value = value[len("github.com/"):]
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !repositoryPart.MatchString(parts[0]) || !repositoryPart.MatchString(parts[1]) {
		return "", fmt.Errorf("cannot resolve repository identity")
	}
	return strings.Join(parts, "/"), nil
}

func (e *Engine) repositoryAccess(ctx context.Context, tool string, args []string) error {
	if len(e.Policy.RepositoryAllowlist) == 0 {
		return nil
	}
	if tool == "git" {
		return e.pushAccess(ctx, args)
	}
	if tool != "gh" {
		return nil
	}
	flags, pos := prSpec.scan(args)
	if len(pos) > 0 && !ghCore[pos[0]] {
		return fmt.Errorf("cannot resolve GitHub alias or extension; use gh pr directly")
	}
	if len(pos) < 2 || pos[0] != "pr" || (pos[1] != "create" && pos[1] != "comment") {
		return nil
	}
	// A PR URL selects its own repository, even with -R. Check both selectors.
	selected := false
	for _, f := range flags {
		if f.name == "repo" {
			selected = true
			if err := e.Policy.CheckRepository(f.value); err != nil {
				return err
			}
		}
	}
	if len(pos) > 2 && strings.Contains(pos[2], "://") {
		u, err := url.Parse(pos[2])
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return fmt.Errorf("cannot resolve PR repository")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 4 || parts[2] != "pull" {
			return fmt.Errorf("cannot resolve PR repository")
		}
		selected = true
		if err := e.Policy.CheckRepository(strings.Join(parts[:2], "/")); err != nil {
			return err
		}
	}
	if selected {
		return nil
	}
	t, ok, why := e.target(ctx, args)
	if !ok {
		return fmt.Errorf("%s", why)
	}
	return e.Policy.CheckRepository(t.Repo)
}

// pushAccess asks Git for effective push URLs (including pushurl and URL
// rewrites), with the same global options as the eventual push. It never
// invokes push, hooks or the network to discover a destination.
func (e *Engine) pushAccess(ctx context.Context, args []string) error {
	var prefix []string
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		a := args[i]
		if a == "--version" || a == "--help" || a == "-h" {
			return nil
		}
		prefix = append(prefix, a)
		i++
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace" || a == "--config-env":
			if i == len(args) {
				return fmt.Errorf("cannot resolve git global option")
			}
			prefix = append(prefix, args[i])
			i++
		case a == "--no-pager" || a == "--paginate" || a == "--bare" || a == "--no-optional-locks" || strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") || strings.HasPrefix(a, "--config-env=") || strings.HasPrefix(a, "--namespace=") || strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-c"):
		default:
			// Unknown global options could hide a push. Refuse, rather than guessing.
			return fmt.Errorf("cannot resolve git command with global option %s", a)
		}
	}
	if i == len(args) {
		return nil
	}
	if args[i] != "push" {
		r, err := e.Runner.Run(ctx, "git", append(append([]string{}, prefix...), "config", "--get", "alias."+args[i])...)
		if err != nil {
			return fmt.Errorf("cannot resolve Git alias")
		}
		if r.Exit == 0 && strings.TrimSpace(r.Stdout) != "" {
			return fmt.Errorf("cannot resolve Git alias; use git push directly")
		}
		return nil
	}
	i++
	remote := ""
	endFlags := false
	for i < len(args) {
		a := args[i]
		i++
		if a == "--" && !endFlags {
			endFlags = true
			continue
		}
		if !endFlags && strings.HasPrefix(a, "-") {
			switch {
			case a == "--repo":
				if i == len(args) {
					return fmt.Errorf("push --repo needs a value")
				}
				remote = args[i]
				i++
			case strings.HasPrefix(a, "--repo="):
				remote = strings.TrimPrefix(a, "--repo=")
			case a == "--receive-pack" || a == "--exec" || a == "-o" || a == "--push-option":
				if i == len(args) {
					return fmt.Errorf("push option needs a value")
				}
				i++
			case strings.HasPrefix(a, "--receive-pack=") || strings.HasPrefix(a, "--exec=") || strings.HasPrefix(a, "--push-option=") || strings.HasPrefix(a, "--force-with-lease=") || strings.HasPrefix(a, "--signed=") || strings.HasPrefix(a, "-o"):
			case contains([]string{"--all", "--branches", "--prune", "--mirror", "--delete", "--tags", "--follow-tags", "--force", "--force-with-lease", "--force-if-includes", "--no-force-if-includes", "--dry-run", "--porcelain", "--set-upstream", "--verbose", "--quiet", "--progress", "--no-progress", "--no-verify", "--verify", "--atomic", "--no-atomic", "--ipv4", "--ipv6", "--signed", "--no-signed", "--thin", "--no-thin", "-f", "-n", "-u", "-v", "-q", "-d", "-4", "-6"}, a):
			default:
				return fmt.Errorf("cannot resolve push option %s", a)
			}
			continue
		}
		// Git's positional repository takes precedence over --repo.
		remote = a
		break // remaining positional arguments are refspecs
	}
	run := func(extra ...string) (string, bool) {
		r, err := e.Runner.Run(ctx, "git", append(append([]string{}, prefix...), extra...)...)
		return strings.TrimSpace(r.Stdout), err == nil && r.Exit == 0
	}
	if remote == "" {
		branch, _ := run("symbolic-ref", "--quiet", "--short", "HEAD")
		for _, key := range []string{"branch." + branch + ".pushRemote", "remote.pushDefault", "branch." + branch + ".remote"} {
			if v, ok := run("config", "--get", key); ok && v != "" {
				remote = v
				break
			}
		}
		if remote == "" {
			remotes, ok := run("remote")
			if !ok || remotes == "" {
				return fmt.Errorf("cannot resolve push remote")
			}
			all := strings.Fields(remotes)
			remote = "origin"
			if len(all) == 1 {
				remote = all[0]
			}
		}
	}
	urls, ok := run("remote", "get-url", "--push", "--all", remote)
	if !ok {
		// Let Git apply pushInsteadOf/insteadOf to an explicit URL too. -c is
		// process-local; it never changes the repository's config.
		if !strings.Contains(remote, ":") {
			return fmt.Errorf("cannot resolve push remote")
		}
		urls, ok = run("-c", "remote.multica-allowlist-target.url="+remote, "remote", "get-url", "--push", "--all", "multica-allowlist-target")
	}
	if !ok || urls == "" {
		return fmt.Errorf("cannot resolve push destination")
	}
	for _, u := range strings.Split(urls, "\n") {
		if err := e.Policy.CheckRepository(u); err != nil {
			return err
		}
	}
	return nil
}
