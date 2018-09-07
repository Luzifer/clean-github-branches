package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Luzifer/rconfig"

	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

var (
	cfg = struct {
		BranchStaleness  time.Duration `flag:"branch-staleness" default:"2160h" description:"When to see a branch as stale (default 90d)"`
		DeleteStale      bool          `flag:"delete-stale" default:"false" description:"Delete branches after branch-staleness even if ahead of base"`
		DryRun           bool          `flag:"dry-run,n" default:"true" description:"Do a dry-run (take no destructive action)"`
		EvenTimeout      time.Duration `flag:"even-timeout" default:"24h" description:"When to delete a branch which is not ahead of base"`
		GithubToken      string        `flag:"token" default:"" description:"Token to access Github API"`
		Listen           string        `flag:"listen" default:":3000" description:"Port/IP to listen on"`
		LogLevel         string        `flag:"log-level" default:"info" description:"Log level (debug, info, warn, error, fatal)"`
		RepoProcessLimit int           `flag:"process-limit" default:"10" description:"How many repos to process concurrently"`
		RepoRegex        string        `flag:"repo-regex,r" default:"" description:"Regular expression the full repo (user/reponame) must match against"`
		VersionAndExit   bool          `flag:"version" default:"false" description:"Prints current version and exits"`
	}{}

	repoRegex *regexp.Regexp

	version = "dev"
)

func init() {
	rconfig.AutoEnv(true)

	if err := rconfig.ParseAndValidate(&cfg); err != nil {
		log.Fatalf("Unable to parse commandline options: %s", err)
	}

	if cfg.VersionAndExit {
		fmt.Printf("git-changerelease %s\n", version)
		os.Exit(0)
	}

	if l, err := log.ParseLevel(cfg.LogLevel); err != nil {
		log.WithError(err).Fatal("Unable to parse log level")
	} else {
		log.SetLevel(l)
	}
}

func main() {
	var err error
	if repoRegex, err = regexp.Compile(cfg.RepoRegex); err != nil {
		log.WithError(err).Fatal("Repo-Regex does not compile")
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: cfg.GithubToken},
	)
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	var (
		logger = log.WithField("dry-run", cfg.DryRun)
		repos  = make(chan *github.Repository, 100)
		wg     = new(sync.WaitGroup)
	)

	defer close(repos)

	// Process repos found
	go repoWalk(ctx, logger, client, wg, repos)

	// Find repos for users
	wg.Add(1)
	go findRepos(ctx, logger, client, wg, repos)

	wg.Wait()
	log.Info("Done.")
}

func analysePullRequests(logger *log.Entry, prs []*github.PullRequest, b *github.Branch) (hasValidMerge, hasOpenPR bool) {
	for _, pr := range prs {
		if pr.GetHead().GetSHA() != b.GetCommit().GetSHA() {
			// Does not seem to be the PR for this branch
			continue
		}

		if pr.GetState() == "open" {
			// Is an open PR, don't touch
			logger.Debug("Branch has an open PR")
			hasOpenPR = true
			continue
		}

		if !pr.GetMerged() && pr.GetMergeCommitSHA() == "" {
			// Is not merged but closed: Don't touch! That's too hot...
			logger.Warn("Branch has a closed but not merged PR")
			continue
		}

		if pr.GetMerged() || pr.GetMergeCommitSHA() != "" {
			hasValidMerge = true
		}
	}

	return hasValidMerge, hasOpenPR
}

func deleteBranch(ctx context.Context, logger *log.Entry, client *github.Client, repo *github.Repository, b *github.Branch, reason string) {
	logger.WithField("reason", reason).Info("Deleting branch")

	if !cfg.DryRun {
		if _, err := client.Git.DeleteRef(ctx, repo.Owner.GetLogin(), repo.GetName(), strings.Join([]string{"heads", b.GetName()}, "/")); err != nil {
			logger.WithError(err).Error("Could not delete branch")
		}
	}
}

func fetchBranchesForRepo(ctx context.Context, client *github.Client, repo *github.Repository) ([]*github.Branch, error) {
	var (
		branches = []*github.Branch{}
		lo       = github.ListOptions{}
	)

	// Fetch all branches for the repo
	for {
		b, resp, err := client.Repositories.ListBranches(ctx, repo.Owner.GetLogin(), repo.GetName(), &lo)
		if err != nil {
			return nil, errors.Wrap(err, "Could not fetch branches")
		}
		branches = append(branches, b...)
		if resp.NextPage == 0 {
			break
		}
		lo.Page = resp.NextPage
	}

	return branches, nil
}

func fetchPullRequestsForBranch(ctx context.Context, client *github.Client, repo *github.Repository, b *github.Branch) ([]*github.PullRequest, error) {
	var (
		prs = []*github.PullRequest{}
		lo  = github.ListOptions{}
	)

	fetch := func(owner, name string) error {
		for {
			p, resp, err := client.PullRequests.List(ctx, owner, name, &github.PullRequestListOptions{
				State:       "all",
				Head:        strings.Join([]string{repo.Owner.GetLogin(), b.GetName()}, ":"),
				Base:        repo.GetDefaultBranch(),
				ListOptions: lo,
			})
			if err != nil {
				return err
			}
			prs = append(prs, p...)
			if resp.NextPage == 0 {
				break
			}
			lo.Page = resp.NextPage
		}
		return nil
	}

	if err := fetch(repo.Owner.GetLogin(), repo.GetName()); err != nil {
		return nil, errors.Wrap(err, "Could not fetch pull-requests")
	}
	if repo.GetFork() {
		// Re-fetch the repo in order to have parent information included
		var err error
		if repo, _, err = client.Repositories.Get(ctx, repo.Owner.GetLogin(), repo.GetName()); err != nil {
			return nil, errors.Wrap(err, "Could not update repo to fetch parent information")
		}
		if err := fetch(repo.GetParent().GetOwner().GetLogin(), repo.GetParent().GetName()); err != nil {
			return nil, errors.Wrap(err, "Could not fetch pull-requests")
		}
	}

	return prs, nil
}

func findRepos(ctx context.Context, logger *log.Entry, client *github.Client, wg *sync.WaitGroup, repos chan *github.Repository) {
	defer wg.Done()

	var (
		userRepos []*github.Repository
		lo        = github.ListOptions{}
	)

	for {
		ur, resp, err := client.Repositories.List(ctx, "", &github.RepositoryListOptions{
			ListOptions: lo,
		})
		if err != nil {
			logger.WithError(err).Error("Unable to fetch repos")
			return
		}
		userRepos = append(userRepos, ur...)
		if resp.NextPage == 0 {
			break
		}
		lo.Page = resp.NextPage
	}

	wg.Add(len(userRepos))
	for _, r := range userRepos {
		if !repoRegex.MatchString(r.GetFullName()) {
			logger.WithField("repo", r.GetFullName()).Debug("Repo is filtered by user parameter")
			wg.Done()
			continue
		}
		if r.GetArchived() {
			logger.WithField("repo", r.GetFullName()).Debug("Repo is archived, not processing")
			wg.Done()
			continue
		}
		repos <- r
	}
}

func processRepo(ctx context.Context, logger *log.Entry, client *github.Client, repo *github.Repository) error {
	logger.Debug("Fetching branches")

	branches, err := fetchBranchesForRepo(ctx, client, repo)
	if err != nil {
		return err
	}

	// Iterate and look at every branch
	for _, b := range branches {
		branchLogger := logger.WithField("branch", b.GetName())

		// Do never touch the default branch
		if b.GetName() == repo.GetDefaultBranch() {
			branchLogger.Debug("Default branch, don't touch")
			continue
		}

		// Fetch full branch information containing commit information
		if b, _, err = client.Repositories.GetBranch(ctx, repo.Owner.GetLogin(), repo.GetName(), b.GetName()); err != nil {
			branchLogger.WithError(err).Error("Could not update branch to fetch commit info")
			continue
		}

		// Fetch all PRs for this repo and if it is a fork also for the fork
		prs, err := fetchPullRequestsForBranch(ctx, client, repo, b)
		if err != nil {
			branchLogger.WithError(err).Error("Could not fetch pull-requests")
			continue
		}

		// Check whether the branch is ahead of the base branch
		branchLogger.WithFields(log.Fields{"base": repo.GetDefaultBranch(), "head": b.GetName()}).Debug("Comparing branch")
		comp, _, err := client.Repositories.CompareCommits(ctx, repo.Owner.GetLogin(), repo.GetName(), repo.GetDefaultBranch(), b.GetName())
		if err != nil {
			branchLogger.WithError(err).Error("Could not compare branches")
			continue
		}
		branchLogger = branchLogger.WithFields(log.Fields{"ahead": comp.GetAheadBy(), "behind": comp.GetBehindBy()})
		branchLogger.Debug("Comparison successful")

		// Determine when the last commit was authored and committed, take newer date
		branchLastModified := time.Duration(math.Min(
			float64(time.Since(b.GetCommit().GetCommit().GetAuthor().GetDate())),
			float64(time.Since(b.GetCommit().GetCommit().GetCommitter().GetDate())),
		))

		// Check all PRs whether they match the branch (head) and are merged
		hasValidMerge, hasOpenPR := analysePullRequests(branchLogger, prs, b)
		if hasOpenPR {
			continue
		}

		switch {
		case hasValidMerge:
			deleteBranch(ctx, branchLogger, client, repo, b,
				"PR exists and is merged")

		case comp.GetAheadBy() == 0 && branchLastModified > cfg.EvenTimeout:
			deleteBranch(ctx, branchLogger, client, repo, b,
				"Branch is even and older than even-timeout")

		case comp.GetAheadBy() > 0 && cfg.DeleteStale && branchLastModified > cfg.BranchStaleness:
			deleteBranch(ctx, branchLogger, client, repo, b,
				"Branch is stale and delete-stale is set")

		case comp.GetAheadBy() > 0 && !cfg.DeleteStale && branchLastModified > cfg.BranchStaleness:
			// Warn target when stale branches are not to be deleted
			branchLogger.Warn("Stale branch found")

		default:
			branchLogger.Debug("No reason for deletion found")
		}

	}

	return nil
}

func repoWalk(ctx context.Context, logger *log.Entry, client *github.Client, wg *sync.WaitGroup, repos <-chan *github.Repository) {
	limiter := make(chan struct{}, cfg.RepoProcessLimit)

	for repo := range repos {
		limiter <- struct{}{}
		go func(ctx context.Context, logger *log.Entry, client *github.Client, wg *sync.WaitGroup, repo *github.Repository, limiter <-chan struct{}) {
			defer func() {
				<-limiter
				wg.Done()
			}()

			repoLogger := logger.WithField("repo", repo.GetFullName())
			if err := processRepo(ctx, repoLogger, client, repo); err != nil {
				repoLogger.WithError(err).Error("Repo processing caused an error")
			}
		}(ctx, logger, client, wg, repo, limiter)
	}
}
