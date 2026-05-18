// TODO: GIT_TERMINAL_PROMPT when pulling/cloning
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	Directory string
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

func checkSpace() error {
	path := "." // current fs

	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return fmt.Errorf("Error with space: %v\n", err)
	}

	// Calculate the remaining space
	// stat.Bfree is the number of free blocks and stat.Bsize is the block size
	remainingSpace := stat.Bfree * uint64(stat.Bsize)
	if remainingSpace == 0 {
		panic("No more free disk space")
	}
	return nil
}

// checkDir checks if a directory exists and is writable by the current user
func checkDir(dir string) (bool, error) {
	// Check if the directory exists
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return false, fmt.Errorf("%s does not exist\n", dir) // Directory does not exist
	}
	if err != nil {
		return false, err // Other error
	}

	// Check if it's a directory
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory\n", dir)
	}

	// Check if the directory is writable
	testFile := filepath.Join(dir, "testfile")
	file, err := os.OpenFile(testFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return false, fmt.Errorf("%s is not writable\n", dir) // Not writable
	}
	file.Close()

	// Clean up test file
	os.Remove(testFile)

	return true, nil // Directory exists and is writable
}

func checkFileReadable(f string) (bool, error) {
	// Check if the file exists
	_, err := os.Stat(f)
	if os.IsNotExist(err) {
		return false, fmt.Errorf("%s does not exist\n", f) // Directory does not exist
	}
	if err != nil {
		return false, err // Other error
	}

	// Check if the directory is readable
	file, err := os.OpenFile(f, os.O_RDONLY, 0666)
	if err != nil {
		return false, fmt.Errorf("%v is not readable\n", file) // Not writable
	}
	file.Close()

	return true, nil // File exists and is readable
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
		req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
	}
	return collectRepositories[GitHubRepository](url, a_token, header)
}

func collectGitLabRepositories(url string, a_token string) ([]GitLabRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITLAB_TOKEN")
		}
		req.Header.Set("PRIVATE-TOKEN", token)
	}
	return collectRepositories[GitLabRepository](url, a_token, header)
}

func collectGiteaRepositories(url string, a_token string) ([]GiteaRepository, error) {
	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITEA_TOKEN")
		}
		req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
	}
	return collectRepositories[GiteaRepository](url, a_token, header)
}

func collectRepositories[T any](url string, a_token string, setHeader func(*http.Request, string)) ([]T, error) {
	var completeRepositories []T
	endOfPaginatedRepos := false
	for i := 1; endOfPaginatedRepos == false; i++ { // can we do this more idiomatically ?
		var repositoriesStore []T

		// Create a new HTTP request
		req, err := http.NewRequest("GET", url+"&page="+strconv.Itoa(i), nil)
		if err != nil {
			return nil, fmt.Errorf("Error creating request: %v\n", err)
		}

		if setHeader != nil {
			// Set the Authorization header with the token
			setHeader(req, a_token)
		}

		// Create a client and send the request
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Error making request: %v\n", err)
		}
		defer resp.Body.Close()

		// Read the response

		if resp.StatusCode == 401 {
			return nil, fmt.Errorf("Status code 401, token is invalid\n")
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Error: received status code %d\n", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("Error reading response body: %s\n", err)
		}
		err = json.Unmarshal(body, &repositoriesStore)
		if err != nil {
			return nil, fmt.Errorf("Error unmarshaling JSON: %v\n", err)
		}
		if len(repositoriesStore) != 0 {
			// unpack
			completeRepositories = append(completeRepositories, repositoriesStore...)
		} else if len(repositoriesStore) == 0 {
			if i == 1 {
				return nil, fmt.Errorf("No repositories found at URL %s\n", url)
			} else if i >= 2 {
				endOfPaginatedRepos = true
			}
		}
	}
	// at the end return the repository list
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

	c := &http.Client{}

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

	var apiUsername string
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

func cloneRepository(repo Repository, dir string) error {
	cmd := exec.Command("git", "clone", repo.GetUrl(), dir)
	cmd.Env = []string{"GIT_TERMINAL_PROMPT=0"}
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("Error running git clone: %v", err)
	}
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("Error cloning repository: %v\n", err)
	}
	return nil
}

func pullRepository(dir string) error {
	// Execute the Symbol’s value as variable is void: pgrep command to find git processes
	cmd := exec.Command("git", "pull")
	cmd.Dir = dir
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("Error running git pull: %v", err)
	}
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("Error pulling repository: %v\n", err)
	}
	return nil
}

func cloneOrPullWorker(wg *sync.WaitGroup, repositoryWithDirChan <-chan RepositoryWithDir) {
	for repo := range repositoryWithDirChan {
		fmt.Printf("Received repo %s, processing\n", repo.Repository.GetName())
			gitDirectoryExists, _ := checkDir(repo.Directory + "/.git")

		if gitDirectoryExists == false { // if no existing .git in that dir then clone
			err := cloneRepository(repo.Repository, repo.Directory)
			if err != nil {
				log.Fatalf("Error cloning repository. Quitting: %v", err)
			}
		} else {
			err := pullRepository(repo.Directory) // if directory exists then pull
			if err != nil {
				log.Fatalf("Error pulling repository. Quitting: %v", err)
			}
		}
	}
  wg.Done()
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
	switch {
	case user != "" && forge == "github":
		url = "https://api.github.com/users/" + user + "/repos?per_page=100"
		dirToAppend = user
	case user != "" && forge == "gitlab" && instanceUrl != "":
		url = instanceUrl + "/api/v4/users/" + user + "/projects?per_page=100"
		dirToAppend = user
	case user != "" && forge == "gitlab":
		url = "https://gitlab.com/api/v4/users/" + user + "/projects?per_page=100"
		dirToAppend = user
	case user != "" && forge == "gitea" && instanceUrl != "":
		url = instanceUrl + "api/v1/users/" + user + "/repos?per_page=100"
		dirToAppend = user
	case user != "" && forge == "gitea":
		url = "https://gitea.com/api/v1/users/" + user + "/repos?per_page=100"
		dirToAppend = user
	case user != "" && (forge != "github" && forge != "gitlab" && forge != "gitea"):
		fmt.Printf("Forge does not exist")
		usage()
		os.Exit(1)
	}
	return url, dirToAppend, nil
}

func retrieveReposUrlFromOrganisation(forge string, organisation string, instanceUrl string) (url string, dirToAppend string, err error) {
	switch {
	case organisation != "" && forge == "github":
		url = "https://api.github.com/orgs/" + organisation + "/repos?per_page=100"
		dirToAppend = organisation
	case organisation != "" && forge == "gitlab" && instanceUrl != "":
		url = instanceUrl + "/api/v4/groups/" + organisation + "/projects?per_page=100&include_subgroups=true"
		dirToAppend = organisation
	case organisation != "" && forge == "gitlab":
		url = "https://gitlab.com/api/v4/groups/" + organisation + "/projects?per_page=100&include_subgroups=true"
		dirToAppend = organisation
	case organisation != "" && forge == "gitea" && instanceUrl != "":
		url = instanceUrl + "api/v1/orgs/" + organisation + "/repos?per_page=100"
		dirToAppend = organisation
	case organisation != "" && forge == "gitea":
		url = "https://gitea.com/api/v1/orgs/" + organisation + "/repos?per_page=100"
		dirToAppend = organisation
	case organisation != "" && (forge != "github" && forge != "gitlab" && forge != "gitea"):
		log.Fatalf("Forge does not exist")
		usage()
		os.Exit(1)
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

func cloneOrPullRepositoryList(collectedRepositories []Repository, forge string, dir string, dirToAppend string, repositoryWithDirChan chan RepositoryWithDir, ignoreForks bool, starsGreater uint) {
	repositoriesFetched := 0
	// * the cloning code
	for _, repository := range collectedRepositories {
		canClone := true
		// can clone?
		if ignoreForks == true && repository.IsFork() {
			canClone = false
		} else if repository.GetStarCount() >= starsGreater {
			canClone = true
		} else if repository.GetStarCount() < starsGreater {
			canClone = false
		}

		repoDir := dir + forge + "/" + dirToAppend + "/" + repository.GetName()

		// TODO: why is this needed?
		// empty directory "abo-abo" alongside "github"
		// os.Remove(dir + dirToAppend)
		if canClone {
			// we don't want to hit the servers with
			// simultaneous requests, so we space them out
			// across a small random interval
			smallSleep()
			checkSpace()
			// sleep for a while if goroutines higher than -t
			// for {
			// 	if uint(runtime.NumGoroutine()) >= goroutines {
			// 		bigSleep()
			// 	} else {
			// 		break
			// 	}
			// }
			fmt.Printf("Sending repository %s with dir %s\n", repository.GetName(), repoDir)
			repositoryWithDirChan <- RepositoryWithDir{Repository: repository, Directory: repoDir}
			repositoriesFetched += 1
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

	case forge == "sourcehut" && token == "":
		fmt.Fprintf(os.Stderr, "OAuth 2 token required for using the SourceHut API")
		usage()
		os.Exit(1)
	}
}

// * main()
func main() {
	// * options parsing
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

	handleOptionErrors(forge, user, organisation, srhtToken, cloneFile)

	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Recovered from panic\n")
			os.Exit(1) // This will handle the panic
		}
	}()

	// * wg setup
	var wg sync.WaitGroup

	// check dir
	if rootDir != "" {
		dirExistsAndIsWritable, err := checkDir(rootDir)
		if !dirExistsAndIsWritable {
			log.Fatalf("Error with directory: %v\n", err)
		}
	}

	// init channel and workers
	var repositoryWithDirChan = make(chan RepositoryWithDir)
	for i := 0; uint(i) < goroutines; i++ {
		wg.Go(func() {cloneOrPullWorker(&wg, repositoryWithDirChan)})
	}
	// * main loop.
	// if given a range of forge + users then iterate over them
	// if not, just single given
	switch {
	case cloneFile != "":
		cloneConfigMap, err := processCloneFile(cloneFile)
		if err != nil {
			log.Fatalf("Error processing clone file: %v", err)
		}
		for forge := range cloneConfigMap {
			for _, user := range cloneConfigMap[forge].Users {

				fmt.Printf("Processing user %s from forge %s\n", user.Name, forge)
				url, dirToAppend, err := retrieveReposUrlFromUser(forge, user.Name, user.InstanceUrl)
				if err != nil {
					log.Fatalf("Error processing user %s: %v\n", user.Name, err)
				}
				os.Mkdir(forge+rootDir+dirToAppend+"/", 0755)
				// call API
				collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, user.Name, url, user.Token, srhtToken)
				if err != nil {
					log.Fatalf("Error processing Forge URL %s: %v", url, err)
				}
				cloneOrPullRepositoryList(collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, user.IgnoreForks, user.StarsGreater)
			}

			// now do the same but for organisations
			for _, organisation := range cloneConfigMap[forge].Organisations {
				url, dirToAppend, err := retrieveReposUrlFromOrganisation(forge, organisation.Name, organisation.InstanceUrl)
				if err != nil {
					log.Fatalf("Error processing organisation: %v", err)
				}
				os.Mkdir(forge+rootDir+dirToAppend+"/", 0755)

				collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, user, url, organisation.Token, srhtToken)
				if err != nil {
					log.Fatalf("Error processing Forge URLs: %v", err)
				}
				cloneOrPullRepositoryList(collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, organisation.IgnoreForks, organisation.StarsGreater)
			}
		}
	case user != "" && cloneFile == "":
		url, dirToAppend, err := retrieveReposUrlFromUser(forge, user, instanceUrl)
		if err != nil {
			log.Fatalf("Error processing user %s: %v", user, err)
		}
		os.Mkdir(forge+rootDir+dirToAppend+"/", 0755)

		// call API
		collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, user, url, "", srhtToken)
		if err != nil {
			log.Fatalf("Error with directory: %v", err)
		}
		cloneOrPullRepositoryList(collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, ignoreForks, starsGreater)

	case organisation != "" && cloneFile == "":
		// one-element list
		url, dirToAppend, err := retrieveReposUrlFromOrganisation(forge, organisation, instanceUrl)
		if err != nil {
			log.Fatalf("Error processing organisation: %v", err)
		}
		os.Mkdir(forge+rootDir+dirToAppend+"/", 0755)

		// call API
		collectedRepositories, err := retrieveRepositoriesFromForgeUrl(forge, user, url, "", srhtToken)
		if err != nil {
			log.Fatalf("Error with directory: %v", err)
		}
		cloneOrPullRepositoryList(collectedRepositories, forge, rootDir, dirToAppend, repositoryWithDirChan, ignoreForks, starsGreater)
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
