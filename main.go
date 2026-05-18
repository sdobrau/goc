// TODO: GIT_TERMINAL_PROMPT when pulling/cloning
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	g_user         = flag.String("u", "", "User to clone from")
	g_forge        = flag.String("f", "", "Forge to clone from")
	g_rootDir      = flag.String("d", "/tmp/", "Root directory to clone to")
	g_srhtToken    = flag.String("T", "", "SourceHut OAuth2 token")
	g_organisation = flag.String("o", "", "Organisation/group to clone from.\nFor GitLab please use the path embedded in the group page URL.")
	g_instanceUrl  = flag.String("i", "", "Instance URL to clone from")
	g_ignoreForks  = flag.Bool("x", false, "Ignore forks")
	g_starsGreater = flag.Uint("s", 0, "Only clone repositories with stars larger than N")
	g_goroutines   = flag.Uint("t", 20, "Number of concurrent goroutines at which to stop spawning.")
	g_cloneFile    = flag.String("F", "", "File to fetch cloning configuration from")
)

type Repository interface {
	GetName() string
	GetUrl() string
	IsFork() bool
	GetStarCount() uint
}

type RepositoryWithDir struct {
	Repository Repository
	Directory  string
}

// * YAML config struct

// FIXME: why need uppercase? why doesn't a getter method work
// v.Forge vs v.Forge() vs v.forge
// this does not work for everything
type CloneConfig struct {
	Forge         string         `yaml:"forge"`
	Users         []User         `yaml:"users"`
	Organisations []Organisation `yaml:"organisations"`
}

type User struct {
	Name         string `yaml:"name"`
	IgnoreForks  bool   `yaml:"ignoreForks"`
	InstanceUrl  string `yaml:"instanceUrl"`
	StarsGreater uint   `yaml:"starsGreater"`
	Token        string `yaml:"token"`
}

type Organisation struct {
	Name         string `yaml:"name"`
	IgnoreForks  bool   `yaml:"ignoreForks"`
	InstanceUrl  string `yaml:"instanceUrl"`
	StarsGreater uint   `yaml:"starsGreater"`
	Token        string `yaml:"token"`
}

// * GitHub struct
type GitHubRepository struct {
	Name            string `json:"name"`
	Url             string `json:"html_url"`
	Fork            bool   `json:"fork"`
	StargazersCount uint   `json:"stargazers_count"`
}

func (r GitHubRepository) GetName() string {
	return r.Name
}

func (r GitHubRepository) GetUrl() string {
	return r.Url
}

func (r GitHubRepository) IsFork() bool {
	return r.Fork
}

func (r GitHubRepository) GetStarCount() uint {
	return r.StargazersCount
}

// * GitLab struct
type GitLabRepository struct {
	Name      string            `json:"name"`
	WebUrl    string            `json:"web_url"`
	Fork      ForkedFromProject `json:"forked_from_project"`
	StarCount uint              `json:"star_count"`
}

type ForkedFromProject struct {
	Name string `json:"name"`
}

func (r GitLabRepository) GetName() string {
	return r.Name
}

func (r GitLabRepository) GetUrl() string {
	return r.WebUrl
}

func (r GitLabRepository) IsFork() bool {
	// if "forked_from_project" exists then it's a fork
	return len(r.Fork.Name) != 0
}

func (r GitLabRepository) GetStarCount() uint {
	return r.StarCount
}

// * Gitea struct
type GiteaRepository struct {
	Name      string `json:"name"`
	WebUrl    string `json:"html_url"`
	Fork      bool   `json:"fork"`
	StarCount uint   `json:"stars_count"`
}

func (r GiteaRepository) GetName() string {
	return r.Name
}

func (r GiteaRepository) GetUrl() string {
	return r.WebUrl
}

func (r GiteaRepository) IsFork() bool {
	return r.Fork
}

func (r GiteaRepository) GetStarCount() uint {
	return r.StarCount
}

// * SourceHut struct
type SourceHutGitRepository struct {
	Id    int64 `json:"id"`
	Owner struct {
		Id            string `json:"id"`
		CanonicalName string `json:"canonicalName"`
	} `json:"owner"`
	Name string `json:"name"`
	Url  string
}

type SourceHutCursor string

var sourceHutRepositoriesQuery = `
query repositories($cursor: Cursor, $filter: Filter) {
  repositories(cursor: $cursor, filter: $filter) {
    results {
      id
      name
      owner { canonicalName }
    }
    cursor
  }
}
`

func (r SourceHutGitRepository) GetName() string {
	return r.Name
}

func (r SourceHutGitRepository) GetUrl() string {
	return r.Url
}

func (r SourceHutGitRepository) IsFork() bool {
	return false
}

func (r SourceHutGitRepository) GetStarCount() uint {
	return 0
}

// * helper functions
func bigSleep() {
	randDelay := time.Duration(int(rand.Intn(2000) + 3000))
	duration := randDelay * time.Millisecond
	time.Sleep(duration)
}

func smallSleep() {
	// we need to convert the random int into a time.Duration
	randDelay := time.Duration(int(rand.Intn(50) + 80))
	duration := randDelay * time.Millisecond
	time.Sleep(duration)
}

func checkSpace(root string) error {
	if root == "" {
		root = "."
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return fmt.Errorf("space check failed for %q: %w", root, err)
	}

	// stat.Bfree is the number of free blocks and stat.Bsize is the block size
	remainingSpace := stat.Bfree * uint64(stat.Bsize)
	if remainingSpace == 0 {
		return fmt.Errorf("no more free disk space on filesystem containing %q", root)
	}
	return nil
}

// checkDir checks if a directory exists and is writable by the current user.
func checkDir(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return false, fmt.Errorf("%s does not exist", dir)
	}
	if err != nil {
		return false, err
	}

	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", dir)
	}

	// Check writability with a temporary file (avoids fixed name collisions).
	f, err := os.CreateTemp(dir, ".goc-writetest-*")
	if err != nil {
		return false, fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)

	return true, nil
}

func checkFileReadable(f string) (bool, error) {
	if _, err := os.Stat(f); os.IsNotExist(err) {
		return false, fmt.Errorf("%s does not exist", f)
	} else if err != nil {
		return false, err
	}

	file, err := os.Open(f)
	if err != nil {
		return false, fmt.Errorf("%s is not readable: %w", f, err)
	}
	_ = file.Close()

	return true, nil
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: goc -f FORGE -u USER|-o ORGANISATION|-F FILE\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
	os.Exit(1)
}

// * Wrappers for collectRepositories

func collectGitHubRepositories(url string, a_token string) ([]GitHubRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITHUB_TOKEN")
		}
		if token != "" {
			req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
		}
	}
	return collectRepositories[GitHubRepository](url, a_token, header)
}

func collectGitLabRepositories(url string, a_token string) ([]GitLabRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITLAB_TOKEN")
		}
		if token != "" {
			req.Header.Set("PRIVATE-TOKEN", token)
		}
	}
	return collectRepositories[GitLabRepository](url, a_token, header)
}

func collectGiteaRepositories(url string, a_token string) ([]GiteaRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITEA_TOKEN")
		}
		if token != "" {
			req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
		}
	}
	return collectRepositories[GiteaRepository](url, a_token, header)
}

func collectRepositories[T any](url string, a_token string, setHeader func(*http.Request, string)) ([]T, error) {
	var completeRepositories []T

	client := &http.Client{Timeout: 30 * time.Second}
	for page := 1; ; page++ {
		var repositoriesStore []T

		req, err := http.NewRequest("GET", url+"&page="+strconv.Itoa(page), nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("User-Agent", "goc")

		if setHeader != nil {
			setHeader(req, a_token)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("making request: %w", err)
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading response body: %w", readErr)
		}

		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("status code 401 (unauthorized): token is invalid")
		}
		if resp.StatusCode != http.StatusOK {
			truncated := body
			if len(truncated) > 200 {
				truncated = truncated[:200]
			}
			return nil, fmt.Errorf("received status code %d: %q", resp.StatusCode, string(truncated))
		}

		if err := json.Unmarshal(body, &repositoriesStore); err != nil {
			return nil, fmt.Errorf("unmarshaling JSON: %w", err)
		}

		if len(repositoriesStore) == 0 {
			if page == 1 {
				return nil, fmt.Errorf("no repositories found at URL %s", url)
			}
			break
		}

		completeRepositories = append(completeRepositories, repositoriesStore...)
	}

	return completeRepositories, nil
}

// * SourceHut functions
func filterSourceHutRepositories(repositories []SourceHutGitRepository, apiUsername string) ([]SourceHutGitRepository, error) {
	var filteredRepositories []SourceHutGitRepository

	for _, repository := range repositories {
		if repository.Owner.CanonicalName != apiUsername {
			continue
		}

		r := SourceHutGitRepository{}
		r.Url = fmt.Sprintf("%s/%s", "https://git.sr.ht/", path.Join(repository.Owner.CanonicalName, repository.Name))
		r.Name = repository.Name
		filteredRepositories = append(filteredRepositories, r)
	}
	return filteredRepositories, nil
}

func querySourceHutRepositoriesPage(cursor SourceHutCursor, apiUsername string, a_token string) ([]SourceHutGitRepository, SourceHutCursor, error) {
	queryPath := "https://git.sr.ht/query"

	// for json
	vars := map[string]any{}
	if cursor != "" {
		vars["cursor"] = cursor
	}
	queryBody, err := json.Marshal(map[string]any{
		"query":     sourceHutRepositoriesQuery,
		"variables": vars,
	})
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequest("POST", queryPath, bytes.NewReader(queryBody))
	if err != nil {
		return nil, "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf("bearer %s", a_token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	c := &http.Client{Timeout: 30 * time.Second}

	res, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return nil, "", fmt.Errorf("Unexpected response code %d from SourceHut: %q\n", res.StatusCode, string(body))
	}

	// result from query
	var response struct {
		Errors json.RawMessage `json:"errors"`
		Data   struct {
			Repositories struct {
				Results []SourceHutGitRepository `json:"results"`
				Cursor  string                   `json:"cursor"`
			} `json:"repositories"`
		} `json:"data"`
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, "", err
	}
	if response.Errors != nil {
		return nil, "", fmt.Errorf("SourceHut API returned errors while listing repositories: %s\n", string(response.Errors))
	}

	repos, err := filterSourceHutRepositories(response.Data.Repositories.Results, apiUsername)
	if err != nil {
		return nil, "", err
	}

	return repos, SourceHutCursor(response.Data.Repositories.Cursor), nil
}

func collectSourceHutGitRepositories(user string, token string) ([]SourceHutGitRepository, error) {

	var completeRepositories []SourceHutGitRepository

	apiUsername := user
	if !strings.HasPrefix(user, "~") {
		apiUsername = "~" + user
	}
	// localUsername := strings.TrimPrefix(user, "~")
	// fetch until cursor nil
	var cursor SourceHutCursor
	for {
		reposPage, nextCursor, err := querySourceHutRepositoriesPage(cursor, apiUsername, token)
		if err != nil {
			return nil, err
		}
		cursor = nextCursor
		completeRepositories = append(completeRepositories, reposPage...)
		// stop looping when we hit an empty cursor, no more pages

		if cursor == "" {
			break
		}
	}

	if len(completeRepositories) != 0 {
		return completeRepositories, nil
	} else {
		return nil, fmt.Errorf("No repositories found for user %s\n", user)
	}
}

func cloneRepository(ctx context.Context, repo Repository, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", repo.GetUrl(), dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone %q: %w: %s", repo.GetUrl(), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func pullRepository(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "pull", "--ff-only")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull %q: %w: %s", dir, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func cloneOrPullWorker(ctx context.Context, repositoryWithDirChan <-chan RepositoryWithDir, errChan chan<- error) {
	for {
		select {
		case <-ctx.Done():
			return
		case repo, ok := <-repositoryWithDirChan:
			if !ok {
				return
			}

			fmt.Printf("Received repo %s, processing\n", repo.Repository.GetName())

			if err := os.MkdirAll(filepath.Dir(repo.Directory), 0755); err != nil {
				if errChan != nil {
					errChan <- fmt.Errorf("creating parent dir for %q: %w", repo.Directory, err)
				}
				continue
			}

			gitDir := filepath.Join(repo.Directory, ".git")
			info, statErr := os.Stat(gitDir)
			hasGitDir := statErr == nil && info.IsDir()

			if !hasGitDir {
				if statErr != nil && !os.IsNotExist(statErr) {
					if errChan != nil {
						errChan <- fmt.Errorf("stat %q: %w", gitDir, statErr)
					}
					continue
				}

				if err := cloneRepository(ctx, repo.Repository, repo.Directory); err != nil {
					if errChan != nil {
						errChan <- err
					}
				}
				continue
			}

			if err := pullRepository(ctx, repo.Directory); err != nil {
				if errChan != nil {
					errChan <- err
				}
			}
		}
	}
}

func configsToMap(configs []CloneConfig) map[string]CloneConfig {
	configMap := make(map[string]CloneConfig)
	for _, config := range configs {
		configMap[config.Forge] = config // Using Forge as the key
	}
	return configMap
}

// * logic functions
func retrieveReposUrlFromUser(forge string, user string, instanceUrl string) (url string, dirToAppend string, err error) {
	if user == "" {
		return "", "", fmt.Errorf("user is empty")
	}

	base := strings.TrimRight(instanceUrl, "/")
	dirToAppend = user

	switch forge {
	case "github":
		url = "https://api.github.com/users/" + user + "/repos?per_page=100"
	case "gitlab":
		if base != "" {
			url = base + "/api/v4/users/" + user + "/projects?per_page=100"
		} else {
			url = "https://gitlab.com/api/v4/users/" + user + "/projects?per_page=100"
		}
	case "gitea":
		if base != "" {
			url = base + "/api/v1/users/" + user + "/repos?per_page=100"
		} else {
			url = "https://gitea.com/api/v1/users/" + user + "/repos?per_page=100"
		}
	default:
		return "", "", fmt.Errorf("unsupported forge: %q", forge)
	}

	return url, dirToAppend, nil
}

func retrieveReposUrlFromOrganisation(forge string, organisation string, instanceUrl string) (url string, dirToAppend string, err error) {
	if organisation == "" {
		return "", "", fmt.Errorf("organisation is empty")
	}

	base := strings.TrimRight(instanceUrl, "/")
	dirToAppend = organisation

	switch forge {
	case "github":
		url = "https://api.github.com/orgs/" + organisation + "/repos?per_page=100"
	case "gitlab":
		if base != "" {
			url = base + "/api/v4/groups/" + organisation + "/projects?per_page=100&include_subgroups=true"
		} else {
			url = "https://gitlab.com/api/v4/groups/" + organisation + "/projects?per_page=100&include_subgroups=true"
		}
	case "gitea":
		if base != "" {
			url = base + "/api/v1/orgs/" + organisation + "/repos?per_page=100"
		} else {
			url = "https://gitea.com/api/v1/orgs/" + organisation + "/repos?per_page=100"
		}
	default:
		return "", "", fmt.Errorf("unsupported forge: %q", forge)
	}

	return url, dirToAppend, nil
}

func processCloneFile(cloneFile string) (map[string]CloneConfig, error) {
	cloneFilePath, err := filepath.Abs(cloneFile)
	if err != nil {
		return nil, err
	}
	isReadable, err := checkFileReadable(cloneFilePath)
	if err != nil {
		return nil, fmt.Errorf("Error parsing yaml, File is not readable error: %v\n", err)
	} else if isReadable {
		data, err := os.ReadFile(cloneFilePath)
		if err != nil {

			return nil, fmt.Errorf("Error reading file: %v\n", err)
		}

		var cloneConfigs []CloneConfig
		err = yaml.Unmarshal(data, &cloneConfigs)
		if err != nil {
			return nil, fmt.Errorf("Error unmarshaling the YAML: %v\n", err)
		}
		// populate the configmap with appropriate values
		cloneConfigMap := configsToMap(cloneConfigs)
		// for each configured forge
		return cloneConfigMap, nil
	}
	return nil, err // ?
}

func retrieveRepositoriesFromForgeUrl(forge string, user string, url string, a_token, srhtToken string) ([]Repository, error) {
	var collectedRepositories []Repository
	switch {
	case forge == "github":
		repositories, err := collectGitHubRepositories(url, a_token)
		if err != nil {
			return nil, fmt.Errorf("Error collecting GitHub repositories: %v\n", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)

		}
	case forge == "gitlab":
		repositories, err := collectGitLabRepositories(url, a_token)
		if err != nil {
			return nil, fmt.Errorf("Error collecting GitLab repositories: %v\n", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)
		}
	case forge == "gitea":
		repositories, err := collectGiteaRepositories(url, a_token)
		if err != nil {
			return nil, fmt.Errorf("Error collecting Gitea repositories: %v\n", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)
		}
	case forge == "sourcehut" && srhtToken != "":
		repositories, err := collectSourceHutGitRepositories(user, srhtToken)
		if err != nil {
			return nil, fmt.Errorf("Error collecting SourceHut repositories: %v\n", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)
		}
	}
	return collectedRepositories, nil
}

func cloneOrPullRepositoryList(ctx context.Context, collectedRepositories []Repository, forge string, rootDir string, dirToAppend string, repositoryWithDirChan chan<- RepositoryWithDir, ignoreForks bool, starsGreater uint, errChan chan<- error) {
	repositoriesFetched := 0
	for _, repository := range collectedRepositories {
		if ignoreForks && repository.IsFork() {
			continue
		}
		if repository.GetStarCount() < starsGreater {
			continue
		}

		repoDir := filepath.Join(rootDir, forge, dirToAppend, repository.GetName())

		// space out requests a bit to be gentle to servers
		smallSleep()
		if err := checkSpace(rootDir); err != nil {
			if errChan != nil {
				errChan <- err
			}
			return
		}

		fmt.Printf("Sending repository %s with dir %s\n", repository.GetName(), repoDir)
		select {
		case <-ctx.Done():
			return
		case repositoryWithDirChan <- RepositoryWithDir{Repository: repository, Directory: repoDir}:
			repositoriesFetched++
		}
	}
	fmt.Printf("Finished fetching %d repos to %s\n", repositoriesFetched, dirToAppend)
}

func handleOptionErrors(forge string, user string, organisation string, token string, cloneFile string) {
	switch {
	case cloneFile != "" && (user != "" || organisation != ""):
		fmt.Fprintf(os.Stderr, "Please specify only one of: -F or -u|-o")
		usage()
		os.Exit(1)
	case forge == "" && cloneFile == "":
		fmt.Fprintf(os.Stderr, "Please specify a forge with -f")
		usage()
		os.Exit(1)
	case user != "" && organisation != "":
		fmt.Fprintf(os.Stderr, "Don't use both -u and -o")
		usage()
		os.Exit(1)
	case forge == "sourcehut" && token == "":
		fmt.Fprintf(os.Stderr, "OAuth 2 token required for using the SourceHut API")
		usage()
		os.Exit(1)
	case user == "" && organisation == "" && cloneFile == "":
		fmt.Fprintf(os.Stderr, "Insert a user with -u or organisation with -o or clone file with -F")
		usage()
		os.Exit(1)
		// case starsGreater < 0:
		// 	fmt.Println("Don't use a negative value for starsGreater")
		// 	usage()
		// 	os.Exit(1)
	}
}

// * main()
func main() {
	flag.Parse()
	user := *g_user
	forge := *g_forge
	instanceUrl := *g_instanceUrl
	organisation := *g_organisation
	rootDir := *g_rootDir
	srhtToken := *g_srhtToken
	ignoreForks := *g_ignoreForks
	starsGreater := *g_starsGreater
	goroutines := *g_goroutines
	cloneFile := *g_cloneFile

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handleOptionErrors(forge, user, organisation, srhtToken, cloneFile)

	if rootDir != "" {
		dirExistsAndIsWritable, err := checkDir(rootDir)
		if !dirExistsAndIsWritable {
			log.Fatalf("Error with directory: %v", err)
		}
	}

	repositoryWithDirChan := make(chan RepositoryWithDir, 128)
	errChan := make(chan error, 128)

	var hadErr atomic.Bool
	var errWg sync.WaitGroup
	errWg.Add(1)
	go func() {
		defer errWg.Done()
		for err := range errChan {
			hadErr.Store(true)
			log.Printf("Error: %v", err)
		}
	}()

	var wg sync.WaitGroup
	for i := uint(0); i < goroutines; i++ {
		wg.Go(func() {
			cloneOrPullWorker(ctx, repositoryWithDirChan, errChan)
		})
	}

	mkdirBase := func(forge string, dirToAppend string) {
		if err := os.MkdirAll(filepath.Join(rootDir, forge, dirToAppend), 0755); err != nil {
			errChan <- fmt.Errorf("creating directory %q: %w", filepath.Join(rootDir, forge, dirToAppend), err)
		}
	}

	switch {
	case cloneFile != "":
		cloneConfigMap, err := processCloneFile(cloneFile)
		if err != nil {
			log.Fatalf("Error processing clone file: %v", err)
		}
		for forge := range cloneConfigMap {
			for _, u := range cloneConfigMap[forge].Users {
				fmt.Printf("Processing user %s from forge %s\n", u.Name, forge)
				url, dirToAppend, err := retrieveReposUrlFromUser(forge, u.Name, u.InstanceUrl)
				if err != nil {
					log.Fatalf("Error processing user %s: %v", u.Name, err)
				}
				mkdirBase(forge, dirToAppend)

				collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, u.Name, url, u.Token, srhtToken)
				if err != nil {
					log.Fatalf("Error processing Forge URL %s: %v", url, err)
				}
				cloneOrPullRepositoryList(ctx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, u.IgnoreForks, u.StarsGreater, errChan)
			}

			for _, org := range cloneConfigMap[forge].Organisations {
				url, dirToAppend, err := retrieveReposUrlFromOrganisation(forge, org.Name, org.InstanceUrl)
				if err != nil {
					log.Fatalf("Error processing organisation: %v", err)
				}
				mkdirBase(forge, dirToAppend)

				collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, org.Name, url, org.Token, srhtToken)
				if err != nil {
					log.Fatalf("Error processing Forge URLs: %v", err)
				}
				cloneOrPullRepositoryList(ctx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, org.IgnoreForks, org.StarsGreater, errChan)
			}
		}

	case user != "":
		url, dirToAppend, err := retrieveReposUrlFromUser(forge, user, instanceUrl)
		if err != nil {
			log.Fatalf("Error processing user %s: %v", user, err)
		}
		mkdirBase(forge, dirToAppend)

		collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, user, url, "", srhtToken)
		if err != nil {
			log.Fatalf("Error retrieving repositories: %v", err)
		}
		cloneOrPullRepositoryList(ctx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, ignoreForks, starsGreater, errChan)

	case organisation != "":
		url, dirToAppend, err := retrieveReposUrlFromOrganisation(forge, organisation, instanceUrl)
		if err != nil {
			log.Fatalf("Error processing organisation: %v", err)
		}
		mkdirBase(forge, dirToAppend)

		collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, organisation, url, "", srhtToken)
		if err != nil {
			log.Fatalf("Error retrieving repositories: %v", err)
		}
		cloneOrPullRepositoryList(ctx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, ignoreForks, starsGreater, errChan)
	}

	close(repositoryWithDirChan)
	wg.Wait()

	close(errChan)
	errWg.Wait()

	if hadErr.Load() {
		os.Exit(1)
	}
	if ctx.Err() != nil {
		os.Exit(130)
	}
}
