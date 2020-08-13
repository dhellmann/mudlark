package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/andygrunwald/go-jira"
	"github.com/google/go-github/v32/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v2"
)

var pullRequestURLPattern *regexp.Regexp

func init() {
	pullRequestURLPattern = regexp.MustCompile("https://github.com/(?P<org>[^/]+)/(?P<repo>[^/]+)/pull/(?P<id>\\d+)")
}

type jiraSettings struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	URL      string `yaml:"url"`
}

type githubSettings struct {
	Token string `yaml:"token"`
}

type appSettings struct {
	Jira          jiraSettings   `yaml:"jira"`
	Github        githubSettings `yaml:"github"`
	DownstreamOrg string         `yaml:"downstreamOrg"`
}

type serviceClients struct {
	jira   *jira.Client
	github *github.Client
}

type repoPRCache struct {
	pullRequests []*github.PullRequest
	commits      map[int][]*github.RepositoryCommit
}

type cache struct {
	pullRequestsByRepo map[string]repoPRCache
}

type pullRequestWithStatus struct {
	pull   *github.PullRequest
	status string
}

const githubPageSize int = 50

func (c *cache) getDetails(settings *appSettings, clients *serviceClients, org, repo string) (*repoPRCache, error) {
	ctx := context.Background()
	repoKey := fmt.Sprintf("%s/%s", org, repo)
	prCache, ok := c.pullRequestsByRepo[repoKey]

	if !ok {
		prCache = repoPRCache{
			pullRequests: []*github.PullRequest{},
			commits:      make(map[int][]*github.RepositoryCommit),
		}
		c.pullRequestsByRepo[repoKey] = prCache

		opts := &github.PullRequestListOptions{
			State: "all",
			ListOptions: github.ListOptions{
				PerPage: githubPageSize,
			},
		}

		for {
			prs, response, err := clients.github.PullRequests.List(ctx, org, repo, opts)
			if err != nil {
				return nil, errors.Wrap(err,
					fmt.Sprintf("could not get pull requests for %s", repoKey))
			}
			prCache.pullRequests = append(prCache.pullRequests, prs...)

			for _, pr := range prs {
				commits, _, err := clients.github.PullRequests.ListCommits(
					ctx, org, repo, *pr.Number, nil)
				if err != nil {
					return nil, errors.Wrap(err,
						fmt.Sprintf("could not get commits for pull request %d", *pr.Number))
				}
				prCache.commits[*pr.Number] = commits
			}

			if response.NextPage == 0 {
				break
			}
			opts.Page = response.NextPage
		}
	}
	return &prCache, nil
}

func loadSettings(filename string) (*appSettings, error) {

	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	result := appSettings{}
	err = yaml.Unmarshal(content, &result)
	if err != nil {
		return nil, err
	}

	if result.Jira.URL == "" {
		return nil, fmt.Errorf("No jira.url found in %s", filename)
	}
	if result.Jira.User == "" {
		return nil, fmt.Errorf("No jira.user found in %s", filename)
	}
	if result.Jira.Password == "" {
		return nil, fmt.Errorf("No jira.password found in %s", filename)
	}

	if result.Github.Token == "" {
		return nil, fmt.Errorf("No github.token found in %s", filename)
	}

	if result.DownstreamOrg == "" {
		return nil, fmt.Errorf("No downstreamOrg found in %s", filename)
	}

	return &result, nil
}

func issueTitleLine(issue *jira.Issue, jiraURL string) string {
	return fmt.Sprintf("%s (%s) %s/browse/%s %q",
		issue.Fields.Type.Name,
		issue.Fields.Status.Name,
		jiraURL,
		issue.Key,
		issue.Fields.Summary,
	)
}

func uniqueStrings(in []string) []string {
	out := []string{}
	keys := make(map[string]bool)
	for _, s := range in {
		if _, ok := keys[s]; ok {
			continue
		}
		keys[s] = true
		out = append(out, s)
	}
	return out
}

func getLinks(issue *jira.Issue) []string {
	results := []string{}

	results = append(results,
		pullRequestURLPattern.FindAllString(issue.Fields.Description, -1)...)

	if issue.Fields.Comments != nil {
		for _, comment := range issue.Fields.Comments.Comments {
			results = append(results,
				pullRequestURLPattern.FindAllString(comment.Body, -1)...)
		}
	}

	return uniqueStrings(results)
}

func getPRStatus(settings *appSettings, clients *serviceClients, pullRequest *github.PullRequest) (pullRequestWithStatus, error) {
	result := pullRequestWithStatus{pull: pullRequest}
	ctx := context.Background()
	result.status = *pullRequest.State
	isMerged, _, err := clients.github.PullRequests.IsMerged(ctx,
		*pullRequest.Base.Repo.Owner.Login, *pullRequest.Base.Repo.Name, *pullRequest.Number)
	if err != nil {
		return result, errors.Wrap(err,
			fmt.Sprintf("could not fetch merge status of pull request %d",
				*pullRequest.Number))
	}
	if isMerged {
		result.status = "merged"
	}
	if result.status == "open" {
		result.status = "OPEN"
	}
	return result, nil
}

func showPRStatus(settings *appSettings, clients *serviceClients, pullRequest pullRequestWithStatus, prefix string) {
	fmt.Printf("%s on %s %s: %s \"%s\"\n",
		prefix,
		*pullRequest.pull.Base.Ref,
		pullRequest.status,
		*pullRequest.pull.HTMLURL,
		*pullRequest.pull.Title,
	)
}

func processLinks(settings *appSettings, clients *serviceClients, cache *cache, links []string, indent string) error {

	ctx := context.Background()
	for _, url := range links {

		// parse the URL to find the args we need for interacting with
		// github's API
		match := pullRequestURLPattern.FindStringSubmatch(url)
		org := match[1]
		repo := match[2]
		idStr := match[3]
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not convert pull request id %q to integer", idStr))
		}

		pullRequest, _, err := clients.github.PullRequests.Get(ctx, org, repo, id)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not fetch pull request %q", idStr))
		}

		prWithStatus, err := getPRStatus(settings, clients, pullRequest)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not get status of %s", *pullRequest.HTMLURL))
		}

		if org == settings.DownstreamOrg {
			showPRStatus(settings, clients, prWithStatus,
				fmt.Sprintf("%s  downstream", indent))
			continue
		}

		showPRStatus(settings, clients, prWithStatus,
			fmt.Sprintf("%s  upstream", indent))

		if prWithStatus.status == "closed" {
			// We don't care if there is no matching downstream PR if
			// we closed the upstream one without merging it.
			continue
		}

		commits, _, err := clients.github.PullRequests.ListCommits(
			ctx, org, repo, id, nil)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not list commits in pull request %q", idStr))
		}

		otherIDs := make(map[int]bool)
		for _, c := range commits {
			otherPRs, _, err := clients.github.PullRequests.ListPullRequestsWithCommit(
				ctx, settings.DownstreamOrg, repo, *c.SHA, nil)
			if err != nil {
				return errors.Wrap(err, "could not find downstream pull requests")
			}

			// look for pull requests containing the same commits via
			// the github API
			for _, otherPR := range otherPRs {
				if *otherPR.HTMLURL == url {
					// the API returns our own PR even when we ask
					// for the ones from the downstream PR
					continue
				}
				if _, ok := otherIDs[*otherPR.Number]; ok {
					continue
				}
				otherIDs[*otherPR.Number] = true

				otherWithStatus, err := getPRStatus(settings, clients, otherPR)
				if err != nil {
					return errors.Wrap(err,
						fmt.Sprintf("could not get status of %s", *otherPR.HTMLURL))
				}
				showPRStatus(settings, clients, otherWithStatus,
					fmt.Sprintf("%s    downstream", indent))
			}

			// look in the cache for commit messages that include the
			// SHA, indicating a reference during a cherry-pick
			if len(otherIDs) == 0 {
				cachedDetails, err := cache.getDetails(settings, clients,
					settings.DownstreamOrg, repo)
				if err != nil {
					return errors.Wrap(err,
						fmt.Sprintf("could not build cache of details for %s/%s",
							settings.DownstreamOrg, repo))
				}
				for _, pr := range cachedDetails.pullRequests {
					for _, otherCommit := range cachedDetails.commits[*pr.Number] {
						if strings.Contains(*otherCommit.Commit.Message, *c.SHA) {
							if _, ok := otherIDs[*pr.Number]; ok {
								continue
							}
							otherIDs[*pr.Number] = true
							otherWithStatus, err := getPRStatus(settings, clients, pr)
							if err != nil {
								return errors.Wrap(err,
									fmt.Sprintf("could not get status of %s", *pr.HTMLURL))
							}
							showPRStatus(settings, clients, otherWithStatus,
								fmt.Sprintf("%s    downstream", indent))
						}
					}
				}
			}
		}

		if len(otherIDs) == 0 {
			fmt.Printf("%s    downstream: no matching pull requests found in %s/%s\n",
				indent, settings.DownstreamOrg, repo,
			)
			continue
		}
	}
	return nil
}

func processOneIssue(settings *appSettings, clients *serviceClients, cache *cache, issueID string, indent string) error {
	issue, _, err := clients.jira.Issue.Get(issueID, nil)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("error processing issue %q", issueID))
	}
	fmt.Printf("\n%s%s\n", indent, issueTitleLine(issue, settings.Jira.URL))

	// processLinks(settings, clients, cache, getLinks(issue))
	links := getLinks(issue)
	if len(links) == 0 {
		fmt.Printf("%s  no github links found\n", indent)
	} else {
		processLinks(settings, clients, cache, links, indent+"  ")
	}

	switch issue.Fields.Type.Name {
	case "Epic", "Feature":
		searchOptions := jira.SearchOptions{
			Expand: "comments",
		}
		searchTerm := "Epic Link"
		if issue.Fields.Type.Name == "Feature" {
			searchTerm = "Parent Link"
		}
		search := fmt.Sprintf("\"%s\" = %s", searchTerm, issueID)
		children, _, err := clients.jira.Issue.Search(search, &searchOptions)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not find sub-tickets related to %s", issueID))
		}

		for _, child := range children {
			err := processOneIssue(settings, clients, cache, child.Key, indent+"  ")
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("could not process %s", child.Key))
			}
		}
	case "Story":
		for _, task := range issue.Fields.Subtasks {
			err := processOneIssue(settings, clients, cache, task.Key, indent+"  ")
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("could not process %s", task.Key))
			}
		}
	default:
	}
	return nil
}

// fileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func main() {
	configDir, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not get default for config file name: %v", err)
		os.Exit(1)
	}
	configFilenameDefault := filepath.Join(configDir, "mudlark", "config.yml")

	configFilename := flag.String("config", configFilenameDefault,
		"the configuration file name")

	flag.Parse()

	if *configFilename == "" {
		fmt.Fprintf(os.Stderr, "Please specify the -config file name")
		os.Exit(1)
	}

	if !fileExists(*configFilename) {
		template, _ := yaml.Marshal(appSettings{})
		fmt.Fprintf(os.Stderr, "Please create %s containing\n\n%s\n",
			*configFilename,
			string(template),
		)
		os.Exit(1)
	}

	settings, err := loadSettings(*configFilename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not load config file %s: %v", *configFilename, err)
		os.Exit(1)
	}

	tp := jira.BasicAuthTransport{
		Username: settings.Jira.User,
		Password: settings.Jira.Password,
	}

	jiraClient, err := jira.NewClient(tp.Client(), settings.Jira.URL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not create client: %v", err)
		os.Exit(1)
	}

	ctx := context.Background()
	tokenSource := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: settings.Github.Token},
	)
	oauthClient := oauth2.NewClient(ctx, tokenSource)
	githubClient := github.NewClient(oauthClient)

	clients := &serviceClients{
		jira:   jiraClient,
		github: githubClient,
	}

	cache := &cache{
		pullRequestsByRepo: make(map[string]repoPRCache),
	}

	for _, issueID := range flag.Args() {
		err := processOneIssue(settings, clients, cache, issueID, "")
		if err != nil {
			fmt.Printf("ERROR: %s\n", err)
		}
	}

}
