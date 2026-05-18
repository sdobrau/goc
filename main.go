// TODO: GIT_TERMINAL_PROMPT when pulling/cloning
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"sort"
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
	OwnerKey   string
	OwnerStats *RunStats
}

type RunStats struct {
	Listed       atomic.Uint64
	Queued       atomic.Uint64
	NotQueued    atomic.Uint64
	Completed    atomic.Uint64
	Succeeded    atomic.Uint64
	Failed       atomic.Uint64
	Canceled     atomic.Uint64
	Cloned       atomic.Uint64
	Pulled       atomic.Uint64
	SkippedForks atomic.Uint64
	SkippedStars atomic.Uint64
	ErrorsLogged atomic.Uint64
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

func collectGitHubRepositories(ctx context.Context, url string, a_token string) ([]GitHubRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITHUB_TOKEN")
		}
		if token != "" {
			req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
		}
	}
	return collectRepositories[GitHubRepository](ctx, url, a_token, header)
}

func collectGitLabRepositories(ctx context.Context, url string, a_token string) ([]GitLabRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITLAB_TOKEN")
		}
		if token != "" {
			req.Header.Set("PRIVATE-TOKEN", token)
		}
	}
	return collectRepositories[GitLabRepository](ctx, url, a_token, header)
}

func collectGiteaRepositories(ctx context.Context, url string, a_token string) ([]GiteaRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITEA_TOKEN")
		}
		if token != "" {
			req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
		}
	}
	return collectRepositories[GiteaRepository](ctx, url, a_token, header)
}

func collectRepositories[T any](ctx context.Context, url string, a_token string, setHeader func(*http.Request, string)) ([]T, error) {
	var completeRepositories []T

	client := &http.Client{Timeout: 30 * time.Second}
	for page := 1; ; page++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		var repositoriesStore []T

		req, err := http.NewRequestWithContext(ctx, "GET", url+"&page="+strconv.Itoa(page), nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("User-Agent", "goc")

		if setHeader != nil {
			setHeader(req, a_token)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
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

func querySourceHutRepositoriesPage(ctx context.Context, cursor SourceHutCursor, apiUsername string, a_token string) ([]SourceHutGitRepository, SourceHutCursor, error) {
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

	req, err := http.NewRequestWithContext(ctx, "POST", queryPath, bytes.NewReader(queryBody))
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

func collectSourceHutGitRepositories(ctx context.Context, user string, token string) ([]SourceHutGitRepository, error) {

	var completeRepositories []SourceHutGitRepository

	apiUsername := user
	if !strings.HasPrefix(user, "~") {
		apiUsername = "~" + user
	}
	// localUsername := strings.TrimPrefix(user, "~")
	// fetch until cursor nil
	var cursor SourceHutCursor
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reposPage, nextCursor, err := querySourceHutRepositoriesPage(ctx, cursor, apiUsername, token)
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

func cloneOrPullWorker(ctx context.Context, repositoryWithDirChan <-chan RepositoryWithDir, errChan chan<- error, stats *RunStats) {
	// Important: we intentionally do NOT stop the worker on ctx cancellation.
	// For graceful SIGINT handling we want to stop producing new work, close the
	// channel, and let workers finish whatever is already queued/in-flight.
	for repo := range repositoryWithDirChan {
		// fmt.Printf("Received repo %s, processing\n", repo.Repository.GetName())

		markDone := func(s *RunStats, ok bool, canceled bool) {
			if s == nil {
				return
			}
			s.Completed.Add(1)
			if ok {
				s.Succeeded.Add(1)
				return
			}
			if canceled {
				s.Canceled.Add(1)
				return
			}
			s.Failed.Add(1)
		}

		if err := os.MkdirAll(filepath.Dir(repo.Directory), 0755); err != nil {
			if errChan != nil {
				errChan <- fmt.Errorf("creating parent dir for %q: %w", repo.Directory, err)
			}
			markDone(stats, false, false)
			if repo.OwnerStats != nil && repo.OwnerStats != stats {
				markDone(repo.OwnerStats, false, false)
			}
			continue
		}

		gitDir := filepath.Join(repo.Directory, ".git")
		info, statErr := os.Stat(gitDir)
		hasGitDir := statErr == nil && info.IsDir()

		apply := func(f func(s *RunStats)) {
			if stats != nil {
				f(stats)
			}
			if repo.OwnerStats != nil && repo.OwnerStats != stats {
				f(repo.OwnerStats)
			}
		}

		if !hasGitDir {
			if statErr != nil && !os.IsNotExist(statErr) {
				if errChan != nil {
					errChan <- fmt.Errorf("stat %q: %w", gitDir, statErr)
				}
				apply(func(s *RunStats) { markDone(s, false, false) })
				continue
			}

			err := cloneRepository(ctx, repo.Repository, repo.Directory)
			if err != nil {
				canceled := ctx.Err() != nil
				if !canceled && errChan != nil {
					errChan <- err
				}
				apply(func(s *RunStats) { markDone(s, false, canceled) })
				continue
			}

			apply(func(s *RunStats) { s.Cloned.Add(1) })
			apply(func(s *RunStats) { markDone(s, true, false) })
			continue
		}

		err := pullRepository(ctx, repo.Directory)
		if err != nil {
			canceled := ctx.Err() != nil
			if !canceled && errChan != nil {
				errChan <- err
			}
			apply(func(s *RunStats) { markDone(s, false, canceled) })
			continue
		}

		apply(func(s *RunStats) { s.Pulled.Add(1) })
		apply(func(s *RunStats) { markDone(s, true, false) })
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

func retrieveRepositoriesFromForgeUrl(ctx context.Context, forge string, user string, url string, a_token, srhtToken string) ([]Repository, error) {
	var collectedRepositories []Repository
	switch {
	case forge == "github":
		repositories, err := collectGitHubRepositories(ctx, url, a_token)
		if err != nil {
			return nil, fmt.Errorf("collecting GitHub repositories: %w", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)
		}
	case forge == "gitlab":
		repositories, err := collectGitLabRepositories(ctx, url, a_token)
		if err != nil {
			return nil, fmt.Errorf("collecting GitLab repositories: %w", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)
		}
	case forge == "gitea":
		repositories, err := collectGiteaRepositories(ctx, url, a_token)
		if err != nil {
			return nil, fmt.Errorf("collecting Gitea repositories: %w", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)
		}
	case forge == "sourcehut" && srhtToken != "":
		repositories, err := collectSourceHutGitRepositories(ctx, user, srhtToken)
		if err != nil {
			return nil, fmt.Errorf("collecting SourceHut repositories: %w", err)
		}
		for _, repo := range repositories {
			collectedRepositories = append(collectedRepositories, repo)
		}
	default:
		return nil, fmt.Errorf("unsupported forge: %q", forge)
	}
	return collectedRepositories, nil
}

func cloneOrPullRepositoryList(ctx context.Context, collectedRepositories []Repository, forge string, rootDir string, dirToAppend string, repositoryWithDirChan chan<- RepositoryWithDir, ignoreForks bool, starsGreater uint, errChan chan<- error, stats *RunStats, ownerKey string, ownerStats *RunStats) {
	eligibleTotal := 0
	for _, repository := range collectedRepositories {
		if ignoreForks && repository.IsFork() {
			continue
		}
		if repository.GetStarCount() < starsGreater {
			continue
		}
		eligibleTotal++
	}

	repositoriesFetched := 0
	defer func() {
		if ctx.Err() == nil {
			return
		}
		missing := eligibleTotal - repositoriesFetched
		if missing <= 0 {
			return
		}
		if stats != nil {
			stats.NotQueued.Add(uint64(missing))
		}
		if ownerStats != nil && ownerStats != stats {
			ownerStats.NotQueued.Add(uint64(missing))
		}
	}()

	for _, repository := range collectedRepositories {
		if ignoreForks && repository.IsFork() {
			if stats != nil {
				stats.SkippedForks.Add(1)
			}
			if ownerStats != nil && ownerStats != stats {
				ownerStats.SkippedForks.Add(1)
			}
			continue
		}
		if repository.GetStarCount() < starsGreater {
			if stats != nil {
				stats.SkippedStars.Add(1)
			}
			if ownerStats != nil && ownerStats != stats {
				ownerStats.SkippedStars.Add(1)
			}
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

		// fmt.Printf("Sending repository %s with dir %s\n", repository.GetName(), repoDir)
		select {
		case <-ctx.Done():
			return
		case repositoryWithDirChan <- RepositoryWithDir{Repository: repository, Directory: repoDir, OwnerKey: ownerKey, OwnerStats: ownerStats}:
			repositoriesFetched++
			if stats != nil {
				stats.Queued.Add(1)
			}
			if ownerStats != nil && ownerStats != stats {
				ownerStats.Queued.Add(1)
			}
		}
	}
	fmt.Printf("Finished processing %d repos to %s\n", repositoriesFetched, dirToAppend)
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
	startedAt := time.Now()
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

	enqueueCtx, cancelEnqueue := context.WithCancel(context.Background())
	defer cancelEnqueue()

	gitCtx, cancelGit := context.WithCancel(context.Background())
	defer cancelGit()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var gotSignal atomic.Bool
	go func() {
		sig := <-sigCh
		gotSignal.Store(true)
		log.Printf("Received %v: stopping new work; waiting for current operations to finish...", sig)
		cancelEnqueue()

		// If the user hits Ctrl+C again, force-cancel git operations.
		sig = <-sigCh
		log.Printf("Received %v again: forcing shutdown...", sig)
		cancelGit()
	}()

	handleOptionErrors(forge, user, organisation, srhtToken, cloneFile)

	if rootDir != "" {
		dirExistsAndIsWritable, err := checkDir(rootDir)
		if !dirExistsAndIsWritable {
			log.Fatalf("Error with directory: %v", err)
		}
	}

	repositoryWithDirChan := make(chan RepositoryWithDir, 128)
	errChan := make(chan error, 128)
	stats := &RunStats{}

	ownerStatsByKey := map[string]*RunStats{}
	ownerKeys := make([]string, 0, 8)
	getOwnerStats := func(key string) *RunStats {
		if s, ok := ownerStatsByKey[key]; ok {
			return s
		}
		s := &RunStats{}
		ownerStatsByKey[key] = s
		ownerKeys = append(ownerKeys, key)
		return s
	}

	var hadErr atomic.Bool
	var errWg sync.WaitGroup
	errWg.Add(1)
	go func() {
		defer errWg.Done()
		for err := range errChan {
			// Cancellation can be part of a forced shutdown; don't count it as a failure.
			if errors.Is(err, context.Canceled) {
				log.Printf("Canceled: %v", err)
				continue
			}

			hadErr.Store(true)
			stats.ErrorsLogged.Add(1)
			// Avoid spamming the console if many repos fail.
			if stats.ErrorsLogged.Load() <= 20 {
				log.Printf("Error: %v", err)
			}
		}
	}()

	var wg sync.WaitGroup
	for i := uint(0); i < goroutines; i++ {
		wg.Go(func() {
			cloneOrPullWorker(gitCtx, repositoryWithDirChan, errChan, stats)
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

		// Graceful shutdown: once enqueueCtx is canceled, stop iterating.
		for forge := range cloneConfigMap {
			if enqueueCtx.Err() != nil {
				break
			}

			for _, u := range cloneConfigMap[forge].Users {
				if enqueueCtx.Err() != nil {
					break
				}

				ownerKey := fmt.Sprintf("%s/user:%s", forge, u.Name)
				ownerStats := getOwnerStats(ownerKey)

				fmt.Printf("Processing user %s from forge %s\n", u.Name, forge)
				url, dirToAppend, err := retrieveReposUrlFromUser(forge, u.Name, u.InstanceUrl)
				if err != nil {
					log.Fatalf("Error processing user %s: %v", u.Name, err)
				}
				mkdirBase(forge, dirToAppend)

				collectedRepositories, err := retrieveRepositoriesFromForgeUrl(enqueueCtx, forge, u.Name, url, u.Token, srhtToken)
				if err != nil {
					if enqueueCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						log.Printf("Interrupted while listing repositories for %s/%s", forge, u.Name)
						break
					}
					log.Fatalf("Error processing Forge URL %s: %v", url, err)
				}

				stats.Listed.Add(uint64(len(collectedRepositories)))
				ownerStats.Listed.Add(uint64(len(collectedRepositories)))
				cloneOrPullRepositoryList(enqueueCtx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, u.IgnoreForks, u.StarsGreater, errChan, stats, ownerKey, ownerStats)
			}

			for _, org := range cloneConfigMap[forge].Organisations {
				if enqueueCtx.Err() != nil {
					break
				}

				url, dirToAppend, err := retrieveReposUrlFromOrganisation(forge, org.Name, org.InstanceUrl)
				if err != nil {
					log.Fatalf("Error processing organisation: %v", err)
				}
				mkdirBase(forge, dirToAppend)

				ownerKey := fmt.Sprintf("%s/org:%s", forge, org.Name)
				ownerStats := getOwnerStats(ownerKey)

				collectedRepositories, err := retrieveRepositoriesFromForgeUrl(enqueueCtx, forge, org.Name, url, org.Token, srhtToken)
				if err != nil {
					if enqueueCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						log.Printf("Interrupted while listing repositories for %s/%s", forge, org.Name)
						break
					}
					log.Fatalf("Error processing Forge URLs: %v", err)
				}
				stats.Listed.Add(uint64(len(collectedRepositories)))
				ownerStats.Listed.Add(uint64(len(collectedRepositories)))
				cloneOrPullRepositoryList(enqueueCtx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, org.IgnoreForks, org.StarsGreater, errChan, stats, ownerKey, ownerStats)
			}
		}

	case user != "":
		if enqueueCtx.Err() == nil {
			url, dirToAppend, err := retrieveReposUrlFromUser(forge, user, instanceUrl)
			if err != nil {
				log.Fatalf("Error processing user %s: %v", user, err)
			}
			mkdirBase(forge, dirToAppend)

			ownerKey := fmt.Sprintf("%s/user:%s", forge, user)
			ownerStats := getOwnerStats(ownerKey)

			collectedRepositories, err := retrieveRepositoriesFromForgeUrl(enqueueCtx, forge, user, url, "", srhtToken)
			if err != nil {
				if enqueueCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					log.Printf("Interrupted while listing repositories for %s/%s", forge, user)
					break
				}
				log.Fatalf("Error retrieving repositories: %v", err)
			}
			stats.Listed.Add(uint64(len(collectedRepositories)))
			ownerStats.Listed.Add(uint64(len(collectedRepositories)))
			cloneOrPullRepositoryList(enqueueCtx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, ignoreForks, starsGreater, errChan, stats, ownerKey, ownerStats)
		}

	case organisation != "":
		if enqueueCtx.Err() == nil {
			url, dirToAppend, err := retrieveReposUrlFromOrganisation(forge, organisation, instanceUrl)
			if err != nil {
				log.Fatalf("Error processing organisation: %v", err)
			}
			mkdirBase(forge, dirToAppend)

			ownerKey := fmt.Sprintf("%s/org:%s", forge, organisation)
			ownerStats := getOwnerStats(ownerKey)

			collectedRepositories, err := retrieveRepositoriesFromForgeUrl(enqueueCtx, forge, organisation, url, "", srhtToken)
			if err != nil {
				if enqueueCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					log.Printf("Interrupted while listing repositories for %s/%s", forge, organisation)
					break
				}
				log.Fatalf("Error retrieving repositories: %v", err)
			}
			stats.Listed.Add(uint64(len(collectedRepositories)))
			ownerStats.Listed.Add(uint64(len(collectedRepositories)))
			cloneOrPullRepositoryList(enqueueCtx, collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, ignoreForks, starsGreater, errChan, stats, ownerKey, ownerStats)
		}
	}

	close(repositoryWithDirChan)
	wg.Wait()

	close(errChan)
	errWg.Wait()

	elapsed := time.Since(startedAt).Round(10 * time.Millisecond)

	listed := stats.Listed.Load()
	queued := stats.Queued.Load()
	notQueued := stats.NotQueued.Load()
	completed := stats.Completed.Load()
	succeeded := stats.Succeeded.Load()
	failed := stats.Failed.Load()
	canceled := stats.Canceled.Load()
	cloned := stats.Cloned.Load()
	pulled := stats.Pulled.Load()
	skippedForks := stats.SkippedForks.Load()
	skippedStars := stats.SkippedStars.Load()
	errorsLogged := stats.ErrorsLogged.Load()

	remaining := uint64(0)
	if queued > completed {
		remaining = queued - completed
	}

	mode := "completed"
	if gotSignal.Load() {
		mode = "interrupted"
	}

	log.Printf(
		"Run %s in %s. Repos: listed=%d queued=%d notQueued=%d skipped(forks=%d, stars=%d) processed=%d (ok=%d failed=%d canceled=%d) actions(clone=%d pull=%d) pending=%d errorsLogged=%d",
		mode,
		elapsed,
		listed,
		queued,
		notQueued,
		skippedForks,
		skippedStars,
		completed,
		succeeded,
		failed,
		canceled,
		cloned,
		pulled,
		remaining,
		errorsLogged,
	)

	if gotSignal.Load() {
		log.Printf("Graceful shutdown: stopped queueing on SIGINT/SIGTERM and waited for workers to finish.")
	}

	if len(ownerKeys) > 0 {
		sort.Strings(ownerKeys)
		log.Printf("Per-owner breakdown:")
		for _, k := range ownerKeys {
			s := ownerStatsByKey[k]
			if s == nil {
				continue
			}

			kListed := s.Listed.Load()
			kQueued := s.Queued.Load()
			kNotQueued := s.NotQueued.Load()
			kCompleted := s.Completed.Load()
			kSucceeded := s.Succeeded.Load()
			kFailed := s.Failed.Load()
			kCanceled := s.Canceled.Load()
			kCloned := s.Cloned.Load()
			kPulled := s.Pulled.Load()
			kSkippedForks := s.SkippedForks.Load()
			kSkippedStars := s.SkippedStars.Load()

			kPending := uint64(0)
			if kQueued > kCompleted {
				kPending = kQueued - kCompleted
			}

			log.Printf(
				"  %s: listed=%d queued=%d notQueued=%d skipped(forks=%d, stars=%d) processed=%d (ok=%d failed=%d canceled=%d) actions(clone=%d pull=%d) pending=%d",
				k,
				kListed,
				kQueued,
				kNotQueued,
				kSkippedForks,
				kSkippedStars,
				kCompleted,
				kSucceeded,
				kFailed,
				kCanceled,
				kCloned,
				kPulled,
				kPending,
			)
		}
	}

	if hadErr.Load() {
		os.Exit(1)
	}
	if gotSignal.Load() {
		os.Exit(130)
	}
}
