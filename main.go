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

	return results
}

func processLinks(settings *appSettings, clients *serviceClients, links []string) error {
	ctx := context.Background()
	for _, url := range links {

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

		status := *pullRequest.State
		isMerged, _, err := clients.github.PullRequests.IsMerged(ctx, org, repo, id)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not fetch merge status of pull request %q", idStr))
		}
		if isMerged {
			status = "merged"
		}

		if org == settings.DownstreamOrg {
			fmt.Printf("  downstream (%s): %s\n", status, url)
		} else {
			fmt.Printf("  upstream (%s): %s\n", status, url)

			if status == "closed" {
				// We don't care if there is no matching downstream PR
				// if we closed the upstream one without merging it.
				continue
			}

			commits, _, err := clients.github.PullRequests.ListCommits(ctx, org, repo, id, nil)
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

					downstreamStatus := *pullRequest.State
					downstreamIsMerged, _, err := clients.github.PullRequests.IsMerged(ctx,
						settings.DownstreamOrg, repo, *otherPR.Number)
					if err != nil {
						return errors.Wrap(err,
							fmt.Sprintf("could not fetch merge status of pull request %d", *otherPR.Number))
					}
					if downstreamIsMerged {
						downstreamStatus = "merged"
					}

					fmt.Printf("    downstream (%s): %s\n",
						downstreamStatus,
						*otherPR.HTMLURL,
					)
				}
			}

			if len(otherIDs) == 0 {
				fmt.Printf("    downstream: no pull requests found for %s/%s\n",
					settings.DownstreamOrg, repo,
				)
				continue
			}
		}
	}
	return nil
}

func processOneIssue(settings *appSettings, clients *serviceClients, issueID string) error {
	issue, _, err := clients.jira.Issue.Get(issueID, nil)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("error processing issue %q", issueID))
	}
	fmt.Printf("%s\n", issueTitleLine(issue, settings.Jira.URL))

	processLinks(settings, clients, getLinks(issue))

	if issue.Fields.Type.Name == "Epic" {
		searchOptions := jira.SearchOptions{
			Expand: "comments",
		}
		search := fmt.Sprintf("\"Epic Link\" = %s", issueID)
		stories, _, err := clients.jira.Issue.Search(search, &searchOptions)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("could not find stories in epic %s", issueID))
		}

		if len(stories) != 0 {
			for _, story := range stories {
				// The search results do not include comments, so we have to
				// fetch tickets when we need the comments.
				storyDetails, _, err := clients.jira.Issue.Get(story.Key, nil)
				if err != nil {
					return errors.Wrap(err,
						fmt.Sprintf("could not fetch story details for %q", story.Key))
				}
				fmt.Printf("  %s\n", issueTitleLine(storyDetails, settings.Jira.URL))
				processLinks(settings, clients, getLinks(storyDetails))
			}
		}
	}
	return nil
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

	for _, issueID := range flag.Args() {
		err := processOneIssue(settings, clients, issueID)
		if err != nil {
			fmt.Printf("ERROR: %s\n", err)
		}
	}

}
