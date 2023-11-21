# backport

backport automatically backports GitHub pull requests to a release branch.  It
is currently hardcoded for use with [cockroachdb/cockroach], but it might
eventually learn to work with other repositories.

## Usage

backport expects to be run from within a CockroachDB clone.

```
$ backport --help
usage: backport [-c <commit>] [-r <release>] <pull-request>...
   or: backport [--continue|--abort]

backport attempts to automatically backport GitHub pull requests to a
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
  -c,  --commit <commit>    only cherry-pick the mentioned commits
  -r,  --release <release>  select release to backport to
  -b,  --branch <branch>    select the branch to backport to
  -f,  --force              live on the edge
       --help               display this help

Example invocations:

    $ backport 23437
    $ backport 23389 23437 -r 1.1 -c 00c6a87 -c a26506b -c '!a32f4ce'
    $ backport 23437 -b release-23.1.10-rc  # backport to the 'release-23.1.10-rc' branch
    $ backport --continue
    $ backport --abort
```

[cockroachdb/cockroach]: https://github.com/cockroachdb/cockroach
