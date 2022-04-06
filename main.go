package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/google/go-github/v29/github"
	"github.com/spf13/pflag"
	"golang.org/x/oauth2"
)

const usage = `usage: backport [-f] [-c <commit>] [-r <release>] <pull-request>...
   or: backport [--continue|--abort]`

const helpString = `backport attempts to automatically backport GitHub pull requests to a
release branch.

By default, backport will cherry-pick all commits in the specified PRs.
If you explicitly list commits on the command line, backport will
cherry-pick only the mentioned commits.

If manual conflict resolution is required, backport will quit so you
can use standard Git commands to resolve the conflict. After you have
resolved the conflict, resume backporting with 'backport --continue'.
To give up instead, run 'backport --abort'.

To determine what Git remote to push to, backport looks at the value of
the cockroach.remote Git config option. You can set this option by
running 'git config cockroach.remote REMOTE-NAME'.

Options:

      --continue           resume an in-progress backport
      --abort              cancel an in-progress backport
  -c, --commit <commit>    only cherry-pick the mentioned commits
  -r, --release <release>  select release to backport to
  -f, --force              live on the edge
      --help               display this help

Example invocations:

    $ backport 23437
    $ backport 23389 23437 -r 1.1 -c 00c6a87 -c a26506b -c '!a32f4ce'
    $ backport --continue
    $ backport --abort`

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)

		if errors.As(err, new(*github.RateLimitError)) {
			fmt.Fprintln(os.Stderr, `hint: unauthenticated GitHub requests are subject to a very strict rate
limit. Please configure backport with a personal access token:

			$ git config cockroach.githubToken TOKEN

For help creating a personal access token, see https://goo.gl/Ep2E6x.`)
		} else if e := (hintedErr{}); errors.As(err, &e) {
			fmt.Fprintf(os.Stderr, "hint: %s\n", e.hint)
		}

		os.Exit(1)
	}
}

var force bool

func run(ctx context.Context) error {
	var cont, abort, help bool
	var commits []string
	var release string

	pflag.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	pflag.BoolVarP(&help, "help", "h", false, "")
	pflag.BoolVar(&cont, "continue", false, "")
	pflag.BoolVar(&abort, "abort", false, "")
	pflag.BoolVarP(&force, "force", "f", false, "")
	pflag.StringArrayVarP(&commits, "commit", "c", nil, "")
	pflag.StringVarP(&release, "release", "r", "", "")
	pflag.Parse()

	if help {
		return runHelp(ctx)
	}

	if (cont || abort) && len(os.Args) != 2 {
		return errors.New(usage)
	}

	if cont {
		return runContinue(ctx)
	} else if abort {
		return runAbort(ctx)
	}
	return runBackport(ctx, pflag.Args(), commits, release)
}

func runHelp(ctx context.Context) error {
	fmt.Fprintln(os.Stderr, usage)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, helpString)
	return nil
}

func runBackport(ctx context.Context, prArgs, commitArgs []string, release string) error {
	if len(prArgs) == 0 {
		return runHelp(ctx)
	}

	var prNos []int
	for _, prArg := range prArgs {
		prNo, err := strconv.Atoi(prArg)
		if err != nil {
			return fmt.Errorf("%q is not a valid pull request number", prArg)
		}
		prNos = append(prNos, prNo)
	}

	c, err := loadConfig(ctx)
	if err != nil {
		return err
	}

	if ok, err := isBackporting(c); err != nil {
		return err
	} else if ok {
		return errors.New("backport already in progress")
	}

	pullRequests, err := loadPullRequests(ctx, c, prNos)
	if err != nil {
		return err
	}

	if !force {
		for _, pr := range pullRequests {
			if pr.baseBranch != "master" {
				return fmt.Errorf("PR #%d targets %s, not master; are you backporting a backport?",
					pr.number, pr.baseBranch)
			}
		}
	}

	if err := pullRequests.selectCommits(commitArgs); err != nil {
		return err
	}

	if release == "" {
		release, err = getLatestRelease(ctx, c)
		if err != nil {
			return err
		}
	}

	releaseBranch := "release-" + release

	// Order is important here. releaseBranch is fetched last so that we can
	// check it out below using FETCH_HEAD.
	for _, branch := range []string{"master", releaseBranch} {
		err = spawn("git", "fetch", "https://github.com/cockroachdb/cockroach.git",
			"refs/heads/"+branch)
		if err != nil {
			return fmt.Errorf("fetching %q branch: %w", branch, err)
		}
	}

	backportBranch := fmt.Sprintf("backport%s-%s", release, strings.Join(prArgs, "-"))
	err = spawn("git", "checkout", whenForced("--force", "--no-force"),
		whenForced("-B", "-b"), backportBranch, "FETCH_HEAD")
	if err != nil {
		return fmt.Errorf("creating backport branch %q: %w", backportBranch, err)
	}

	query := url.Values{}
	query.Add("expand", "1")
	query.Add("title", pullRequests.title(release))
	query.Add("body", pullRequests.message())
	backportURL := fmt.Sprintf("https://github.com/cockroachdb/cockroach/compare/%s...%s:%s?%s",
		releaseBranch, c.username, backportBranch, query.Encode())

	err = ioutil.WriteFile(c.urlFile(), []byte(backportURL), 0644)
	if err != nil {
		return fmt.Errorf("writing url file: %w", err)
	}

	err = spawn(append([]string{"git", "cherry-pick"}, pullRequests.selectedCommits()...)...)
	if err != nil {
		return hintedErr{
			error: err,
			hint: `Automatic cherry-picking failed. This usually indicates that manual
conflict resolution is required. Run 'backport --continue' to resume
backporting. To give up instead, run 'backport --abort'.`,
		}
	}

	return finalize(c, backportBranch, backportURL)
}

func runContinue(ctx context.Context) error {
	c, err := loadConfig(ctx)
	if err != nil {
		return err
	}

	if ok, err := isBackporting(c); err != nil {
		return err
	} else if !ok {
		return errors.New("no backport in progress")
	}

	if ok, err := isCherryPicking(c); err != nil {
		return err
	} else if ok {
		err = spawn("git", "cherry-pick", "--continue")
		if err != nil {
			return err
		}
	}

	in, err := ioutil.ReadFile(c.urlFile())
	if err != nil {
		return fmt.Errorf("reading url file: %w", err)
	}
	backportURL := string(in)

	matches := regexp.MustCompile(`:(backport.*)\?`).FindStringSubmatch(backportURL)
	if len(matches) == 0 {
		return fmt.Errorf("malformatted url file: %s", backportURL)
	}
	backportBranch := matches[1]

	return finalize(c, backportBranch, backportURL)
}

func runAbort(ctx context.Context) error {
	c, err := loadConfig(ctx)
	if err != nil {
		return err
	}

	if ok, err := isBackporting(c); err != nil {
		return err
	} else if !ok {
		return errors.New("no backport in progress")
	}

	err = os.Remove(c.urlFile())
	if err != nil {
		return fmt.Errorf("removing url file: %w", err)
	}

	if ok, err := isCherryPicking(c); err != nil {
		return err
	} else if ok {
		err = spawn("git", "cherry-pick", "--abort")
		if err != nil {
			return err
		}
	}

	return checkoutPrevious()
}

func finalize(c config, backportBranch, backportURL string) error {
	err := spawn("git", "push", "-u", whenForced("--force", "--no-force"),
		c.remote, fmt.Sprintf("%[1]s:%[1]s", backportBranch))
	if err != nil {
		return fmt.Errorf("pushing branch: %w", err)
	}

	err = os.Remove(c.urlFile())
	if err != nil {
		return fmt.Errorf("removing url file: %w", err)
	}

	err = spawn(browserCmd(backportURL)...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: unable to launch web browser: %s\n", err)
		fmt.Fprintf(os.Stderr, "Submit PR manually at:\n    %s\n", backportURL)
	}

	return checkoutPrevious()
}

func isCherryPicking(c config) (bool, error) {
	_, err := os.Stat(filepath.Join(c.gitDir, "CHERRY_PICK_HEAD"))
	if err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking for in-progress cherry-pick: %w", err)
	}
	return false, nil
}

func isBackporting(c config) (bool, error) {
	_, err := os.Stat(c.urlFile())
	if err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking for in-progress backport: %w", err)
	}
	return false, nil
}

func checkoutPrevious() error {
	branch, err := capture("git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("looking up current branch name: %w", err)
	}
	if !regexp.MustCompile(`^backport\d+`).MatchString(branch) {
		return nil
	}
	if err := spawn("git", "checkout", whenForced("--force", "--no-force"), "-"); err != nil {
		return fmt.Errorf("returning to previous branch: %w", err)
	}
	return nil
}

type config struct {
	ghClient *github.Client
	remote   string
	username string
	gitDir   string
}

func loadConfig(ctx context.Context) (config, error) {
	var c config

	// Determine remote.
	c.remote, _ = capture("git", "config", "--get", "cockroach.remote")
	if c.remote == "" {
		return c, hintedErr{
			error: errors.New("missing cockroach.remote configuration"),
			hint: `set cockroach.remote to the name of the Git remote to push
backports to. For example:

    $ git config cockroach.remote origin
`,
		}
	}

	// Determine username.
	remoteURL, err := capture("git", "remote", "get-url", "--push", c.remote)
	if err != nil {
		return c, fmt.Errorf("determining URL for remote %q: %w", c.remote, err)
	}
	m := regexp.MustCompile(`github.com(:|/)([[:alnum:]\-]+)`).FindStringSubmatch(remoteURL)
	if len(m) != 3 {
		return c, fmt.Errorf("unable to guess GitHub username from remote %q (%s)",
			c.remote, remoteURL)
	} else if m[2] == "cockroachdb" {
		return c, fmt.Errorf("refusing to use unforked remote %q (%s)",
			c.remote, remoteURL)
	}
	c.username = m[2]

	// Build GitHub client.
	var ghAuthClient *http.Client
	ghToken, _ := capture("git", "config", "--get", "cockroach.githubToken")
	if ghToken != "" {
		ghAuthClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: ghToken}))
	}
	c.ghClient = github.NewClient(ghAuthClient)

	// Determine Git directory.
	c.gitDir, err = capture("git", "rev-parse", "--git-dir")
	if err != nil {
		return c, fmt.Errorf("looking up git directory: %w", err)
	}

	return c, nil
}

func (c config) urlFile() string {
	return filepath.Join(c.gitDir, "BACKPORT_URL")
}

func getLatestRelease(ctx context.Context, c config) (string, error) {
	opt := &github.BranchListOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var allBranches []*github.Branch
	for {
		branches, res, err := c.ghClient.Repositories.ListBranches(ctx, "cockroachdb", "cockroach", opt)
		if err != nil {
			return "", fmt.Errorf("discovering release branches: %w", err)
		}
		allBranches = append(allBranches, branches...)
		if res.NextPage == 0 {
			break
		}
		opt.Page = res.NextPage
	}

	var lastRelease string
	for _, branch := range allBranches {
		if !strings.HasPrefix(branch.GetName(), "release-") {
			continue
		}
		lastRelease = strings.TrimPrefix(branch.GetName(), "release-")
	}
	if lastRelease == "" {
		return "", errors.New("unable to determine latest release; try specifying --release")
	}
	return lastRelease, nil
}

type pullRequest struct {
	number          int
	title           string
	body            string
	commits         []string
	selectedCommits []string
	baseBranch      string
}

type pullRequests []pullRequest

func loadPullRequests(ctx context.Context, c config, prNos []int) (pullRequests, error) {
	var prs pullRequests
	for _, prNo := range prNos {
		ghPR, _, err := c.ghClient.PullRequests.Get(ctx, "cockroachdb", "cockroach", prNo)
		if err != nil {
			return nil, fmt.Errorf("fetching PR #%d: %w", prNo, err)
		}
		commits, _, err := c.ghClient.PullRequests.ListCommits(ctx, "cockroachdb", "cockroach", prNo, nil)
		if err != nil {
			return nil, fmt.Errorf("fetching commits from PR #%d: %w", prNo, err)
		}
		pr := pullRequest{
			number:     prNo,
			title:      ghPR.GetTitle(),
			body:       ghPR.GetBody(),
			baseBranch: ghPR.GetBase().GetRef(),
		}
		for _, c := range commits {
			pr.commits = append(pr.commits, c.GetSHA())
			pr.selectedCommits = append(pr.selectedCommits, c.GetSHA())
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

func (prs pullRequests) selectCommits(refs []string) error {
	var includeRefs []string
	var excludeRefs []string
	for _, ref := range refs {
		if strings.HasPrefix(ref, "!") {
			excludeRefs = append(excludeRefs, ref[1:])
		} else {
			includeRefs = append(includeRefs, ref)
		}
	}

	if len(includeRefs) > 0 {
		for i := range prs {
			prs[i].selectedCommits = nil
		}
	}

	for _, ref := range includeRefs {
		var found bool
		for i := range prs {
			for _, commit := range prs[i].commits {
				if strings.HasPrefix(commit, ref) {
					if found {
						return fmt.Errorf("commit ref %q is ambiguous", ref)
					}
					prs[i].selectedCommits = append(prs[i].selectedCommits, commit)
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("commit %q was not found in any of the specified PRs", ref)
		}
	}

	for _, ref := range excludeRefs {
		var found bool
		for i := range prs {
			for j, commit := range prs[i].selectedCommits {
				if strings.HasPrefix(commit, ref) {
					if found {
						return fmt.Errorf("commit ref %q is ambiguous", ref)
					}
					prs[i].selectedCommits = append(prs[i].selectedCommits[:j], prs[i].selectedCommits[j+1:]...)
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("commit %q was not found in any of the specified PRs", ref)
		}
	}

	return nil
}

func (prs pullRequests) selectedCommits() []string {
	var commits []string
	for _, pr := range prs {
		commits = append(commits, pr.selectedCommits...)
	}
	return commits
}

func (prs pullRequests) selectedPRs() pullRequests {
	var selectedPRs []pullRequest
	for _, pr := range prs {
		if len(pr.selectedCommits) > 0 {
			selectedPRs = append(selectedPRs, pr)
		}
	}
	return selectedPRs
}

func (prs pullRequests) title(release string) string {
	prs = prs.selectedPRs()
	if len(prs) == 1 {
		return fmt.Sprintf("release-%s: %s", release, prs[0].title)
	}
	return fmt.Sprintf("release-%s: TODO", release)
}

func (prs pullRequests) message() string {
	prs = prs.selectedPRs()
	var s strings.Builder
	if len(prs) == 1 {
		fmt.Fprintf(&s, "Backport %d/%d commits from #%d.\n",
			len(prs[0].selectedCommits), len(prs[0].commits), prs[0].number)
	} else {
		fmt.Fprintln(&s, "Backport:")
		for _, pr := range prs {
			fmt.Fprintf(&s, "  * %d/%d commits from %q (#%d)\n",
				len(pr.selectedCommits), len(pr.commits), pr.title, pr.number)
		}
		fmt.Fprintln(&s)
		fmt.Fprintln(&s, "Please see individual PRs for details.")
	}
	fmt.Fprintln(&s)
	fmt.Fprintln(&s, "/cc @cockroachdb/release")
	if len(prs) == 1 {
		fmt.Fprintln(&s)
		fmt.Fprintln(&s, "---")
		fmt.Fprintln(&s)
		fmt.Fprintln(&s, prs[0].body)
	}
	return s.String()
}

type hintedErr struct {
	hint string
	error
}

func whenForced(forced, unforced string) string {
	if force {
		return forced
	}
	return unforced
}

func browserCmd(url string) []string {
	var cmd []string
	switch runtime.GOOS {
	case "darwin":
		cmd = append(cmd, "/usr/bin/open")
	case "windows":
		cmd = append(cmd, "cmd", "/c", "start")
	default:
		cmd = append(cmd, "xdg-open")
	}
	cmd = append(cmd, url)
	return cmd
}
