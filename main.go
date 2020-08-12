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
	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v2"
)

var pullRequestURLPattern *regexp.Regexp

const downstreamOrg string = "openshift"

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
	Jira   jiraSettings   `yaml:"jira"`
	Github githubSettings `yaml:"github"`
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

	return &result, nil
}

func issueTitleLine(issue *jira.Issue) string {
	return fmt.Sprintf("%s %s (%s) %q",
		issue.Fields.Type.Name,
		issue.Key,
		issue.Fields.Status.Name,
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

func getPRsForCommit(client *github.Client, org, repo, sha string) ([]*github.PullRequest, error) {
	ctx := context.Background()

	u := fmt.Sprintf("repos/%v/%v/commits/%v/pulls", org, repo, sha)
	req, err := client.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	// TODO: remove custom Accept header when this API fully launches.
	req.Header.Set("Accept", "application/vnd.github.groot-preview+json")

	var pulls []*github.PullRequest
	_, err = client.Do(ctx, req, &pulls)
	if err != nil {
		return nil, err
	}

	return pulls, nil
}

func processLinks(client *github.Client, links []string) error {
	ctx := context.Background()
	for _, url := range links {
		fmt.Printf("  PR: %s\n", url)
		match := pullRequestURLPattern.FindStringSubmatch(url)
		org := match[1]
		repo := match[2]
		idStr := match[3]
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not convert pull request id %q to integer", idStr))
		}
		pullRequest, _, err := client.PullRequests.Get(ctx, org, repo, id)
		if err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("could not fetch pull request %q", idStr))
		}
		if org == downstreamOrg {
			fmt.Printf("    downstream\n")
		} else {
			fmt.Printf("    upstream\n")
			commits, _, err := client.PullRequests.ListCommits(ctx, org, repo, id, nil)
			if err != nil {
				return errors.Wrap(err,
					fmt.Sprintf("could not list commits in pull request %q", idStr))
			}
			for _, c := range commits {
				fmt.Printf("      commit: %s\n", *c.SHA)
				otherPRs, err := getPRsForCommit(client, downstreamOrg, repo, *c.SHA)
				if err != nil {
					return errors.Wrap(err, "could not find downstream pull requests")
				}
				for _, otherPR := range otherPRs {
					if *otherPR.HTMLURL == url {
						// the API returns our own PR even when we ask
						// for the ones from the downstream PR
						continue
					}
					isMerged := "no"
					if otherPR.Merged != nil && *otherPR.Merged {
						isMerged = "yes"
					}
					fmt.Printf("      downstream PR: %d %s\n", *otherPR.Number, isMerged)
					if otherPR.MergedAt != nil {
						fmt.Printf("      %v\n", *otherPR.MergedAt)
					}
				}
			}
		}
		fmt.Printf("    state: %s\n", *pullRequest.State)
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

	searchOptions := jira.SearchOptions{
		Expand: "comments",
	}

	for _, issueID := range flag.Args() {
		issue, _, err := jiraClient.Issue.Get(issueID, nil)
		if err != nil {
			fmt.Printf("ERROR: %s\n", err)
			continue
		}
		fmt.Printf("%s\n", issueTitleLine(issue))

		processLinks(githubClient, getLinks(issue))

		if issue.Fields.Type.Name == "Epic" {
			search := fmt.Sprintf("\"Epic Link\" = %s", issueID)
			stories, _, err := jiraClient.Issue.Search(search, &searchOptions)
			if err != nil {
				fmt.Printf("ERROR finding stories in epic: %s\n", err)
				continue
			}

			if len(stories) != 0 {
				for _, story := range stories {
					// The search results do not include comments, so we have to
					// fetch tickets when we need the comments.
					storyDetails, _, err := jiraClient.Issue.Get(story.Key, nil)
					if err != nil {
						fmt.Printf("ERROR fetching story %s: %s\n", story.Key, err)
						continue
					}
					fmt.Printf("  %s\n", issueTitleLine(storyDetails))
					processLinks(githubClient, getLinks(storyDetails))
				}
			}
		}
	}

}