package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"

	"github.com/andygrunwald/go-jira"
	"gopkg.in/yaml.v2"
)

var urlPattern *regexp.Regexp

func init() {
	urlPattern = regexp.MustCompile("https://github.com/\\S+")
}

type jiraSettings struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	URL      string `yaml:"url"`
}

type appSettings struct {
	Jira jiraSettings `yaml:"jira"`
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

	results = append(results, urlPattern.FindAllString(issue.Fields.Description, -1)...)

	if issue.Fields.Comments != nil {
		for _, comment := range issue.Fields.Comments.Comments {
			results = append(results, urlPattern.FindAllString(comment.Body, -1)...)
		}
	}

	return results
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

		for _, url := range getLinks(issue) {
			fmt.Printf("  link: %s\n", url)
		}

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
					for _, url := range getLinks(storyDetails) {
						fmt.Printf("    link: %s\n", url)
					}

				}
			}
		}
	}

}
