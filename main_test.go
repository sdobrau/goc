package main

// code taken from Claude.

import (
	"testing"
	"os"
	"path/filepath"
	"fmt"
	"reflect"
	"sync"
	"os/exec"
	"net/http"
)

// * helpers

// Compares two CloneConfigs.
func equalConfigs(a, b CloneConfig) bool {
	return a.Forge == b.Forge &&
		reflect.DeepEqual(a.Users, b.Users) &&
		reflect.DeepEqual(a.Organisations, b.Organisations)
}

// * the test functions

// * test functions for helper functions
func TestCheckDir(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() string
		cleanup     func(string)
		expectedOk  bool
		expectError bool
	}{
		{
			name: "Existing writable directory",
			setup: func() string {
				dir, _ := os.MkdirTemp("", "checkdir-valid-*")
				return dir
			},
			cleanup:     func(dir string) { os.RemoveAll(dir) },
			expectedOk:  true,
			expectError: false,
		},
		{
			name: "Non-existent directory",
			setup: func() string {
				return "/nonexistent/path/dir"
			},
			cleanup:     func(dir string) {},
			expectedOk:  false,
			expectError: true,
		},
		{
			name: "Path is a file not a directory",
			setup: func() string {
				f, _ := os.CreateTemp("", "checkdir-file-*")
				f.Close()
				return f.Name()
			},
			cleanup:     func(dir string) { os.Remove(dir) },
			expectedOk:  false,
			expectError: true,
		},
		{
			name: "Non-writable directory",
			setup: func() string {
				dir, _ := os.MkdirTemp("", "checkdir-nowrite-*")
				os.Chmod(dir, 0444)
				return dir
			},
			cleanup: func(dir string) {
				os.Chmod(dir, 0755)
				os.RemoveAll(dir)
			},
			expectedOk:  false,
			expectError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := test.setup()
			defer test.cleanup(dir)

			ok, err := checkDir(dir)

			if ok != test.expectedOk {
				t.Errorf("Expected ok=%v, got ok=%v", test.expectedOk, ok)
			}
			if (err != nil) != test.expectError {
				t.Errorf("Expected error=%v, got err=%v", test.expectError, err)
			}
		})
	}
}

// * test functions for core functionality
func TestHandleOptionErrors(t *testing.T) {
	tests := []struct {
		name         string
		forge        string
		user         string
		organisation string
		token        string
		cloneFile    string
		shouldExit   bool
	}{
		{
			name:         "Valid: GitHub user",
			forge:        "github",
			user:         "torvalds",
			organisation: "",
			token:        "",
			cloneFile:    "",
			shouldExit:   false,
		},
		{
			name:         "Valid: GitHub organisation",
			forge:        "github",
			user:         "",
			organisation: "golang",
			token:        "",
			cloneFile:    "",
			shouldExit:   false,
		},
		{
			name:         "Valid: Clone file only",
			forge:        "",
			user:         "",
			organisation: "",
			token:        "",
			cloneFile:    "config.yaml",
			shouldExit:   false,
		},
		{
			name:         "Valid: SourceHut with token",
			forge:        "sourcehut",
			user:         "~sircmpwn",
			organisation: "",
			token:        "sometoken",
			cloneFile:    "",
			shouldExit:   false,
		},
		{
			name:         "Invalid: Clone file with user",
			forge:        "github",
			user:         "torvalds",
			organisation: "",
			token:        "",
			cloneFile:    "config.yaml",
			shouldExit:   true,
		},
		{
			name:         "Invalid: Clone file with organisation",
			forge:        "github",
			user:         "",
			organisation: "golang",
			token:        "",
			cloneFile:    "config.yaml",
			shouldExit:   true,
		},
		{
			name:         "Invalid: No forge and no clone file",
			forge:        "",
			user:         "torvalds",
			organisation: "",
			token:        "",
			cloneFile:    "",
			shouldExit:   true,
		},
		{
			name:         "Invalid: Both user and organisation",
			forge:        "github",
			user:         "torvalds",
			organisation: "golang",
			token:        "",
			cloneFile:    "",
			shouldExit:   true,
		},
		{
			name:         "Invalid: SourceHut without token",
			forge:        "sourcehut",
			user:         "~sircmpwn",
			organisation: "",
			token:        "",
			cloneFile:    "",
			shouldExit:   true,
		},
		{
			name:         "Invalid: No user, no org, no clone file",
			forge:        "github",
			user:         "",
			organisation: "",
			token:        "",
			cloneFile:    "",
			shouldExit:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.shouldExit {
				// handleOptionErrors calls os.Exit, so we run it
				// in a subprocess and check the exit code
				cmd := exec.Command(os.Args[0], "-test.run=TestHandleOptionErrorsSubprocess")
				cmd.Env = append(os.Environ(),
					"TEST_SUBPROCESS=1",
					"TEST_FORGE="+test.forge,
					"TEST_USER="+test.user,
					"TEST_ORG="+test.organisation,
					"TEST_TOKEN="+test.token,
					"TEST_CLONEFILE="+test.cloneFile,
				)
				err := cmd.Run()
				if exitErr, ok := err.(*exec.ExitError); ok {
					if exitErr.ExitCode() == 0 {
						t.Errorf("Expected non-zero exit code, got 0")
					}
				} else if err == nil {
					t.Errorf("Expected non-zero exit, got nil error")
				}
			} else {
				// should not exit; just call directly
				handleOptionErrors(test.forge, test.user, test.organisation, test.token, test.cloneFile)
			}
		})
	}
}

// TestHandleOptionErrorsSubprocess is a helper test invoked as a subprocess
// by TestHandleOptionErrors for cases that call os.Exit.
func TestHandleOptionErrorsSubprocess(t *testing.T) {
	if os.Getenv("TEST_SUBPROCESS") != "1" {
		return
	}
	handleOptionErrors(
		os.Getenv("TEST_FORGE"),
		os.Getenv("TEST_USER"),
		os.Getenv("TEST_ORG"),
		os.Getenv("TEST_TOKEN"),
		os.Getenv("TEST_CLONEFILE"),
	)
}

func TestProcessCloneFile(t *testing.T) {
	tests := []struct {
		name           string
		yamlContent    string
		createFile     bool
		readable       bool
		expectedOutput map[string]CloneConfig
		expectError    bool
	}{
		{
			name:       "Valid YAML with users and organisations",
			createFile: true,
			readable:   true,
			yamlContent: `
- forge: github
  users:
    - name: torvalds
      ignoreForks: true
      starsGreater: 10
    - name: gvanrossum
  organisations:
    - name: golang
      ignoreForks: false
- forge: gitlab
  users:
    - name: someuser
      instanceUrl: https://gitlab.example.com
      token: mytoken
`,
			expectedOutput: map[string]CloneConfig{
				"github": {
					Forge: "github",
					Users: []User{
						{Name: "torvalds", IgnoreForks: true, StarsGreater: 10},
						{Name: "gvanrossum"},
					},
					Organisations: []Organisation{
						{Name: "golang", IgnoreForks: false},
					},
				},
				"gitlab": {
					Forge: "gitlab",
					Users: []User{
						{Name: "someuser", InstanceUrl: "https://gitlab.example.com", Token: "mytoken"},
					},
				},
			},
			expectError: false,
		},
		{
			name:           "Invalid YAML format",
			createFile:     true,
			readable:       true,
			yamlContent:    "{{{{invalid yaml!!!!",
			expectedOutput: nil,
			expectError:    true,
		},
		{
			name:           "Non-existent file",
			createFile:     false,
			readable:       false,
			yamlContent:    "",
			expectedOutput: nil,
			expectError:    true,
		},
		{
			name:           "Empty file",
			createFile:     true,
			readable:       true,
			yamlContent:    "",
			expectedOutput: map[string]CloneConfig{},
			expectError:    false,
		},
		{
			name:           "Unreadable file",
			createFile:     true,
			readable:       false,
			yamlContent:    "- forge: github\n",
			expectedOutput: nil,
			expectError:    true,
		},
	}

	for _, test := range tests { // for each test 
		t.Run(test.name, func(t *testing.T) { // run the test
			var filePath string

			if test.createFile {
				tmpFile, err := os.CreateTemp("", "cloneconfig-*.yaml")
				if err != nil {
					t.Fatalf("Failed to create temp file: %v", err)
				}
				defer os.Remove(tmpFile.Name())

				// populate it with the yaml
				if _, err := tmpFile.WriteString(test.yamlContent); err != nil {
					t.Fatalf("Failed to write to temp file: %v", err)
				}
				tmpFile.Close()

				if !test.readable {
					os.Chmod(tmpFile.Name(), 0000)
					defer os.Chmod(tmpFile.Name(), 0644)
				}

				filePath = tmpFile.Name()
			} else {
				filePath = filepath.Join(os.TempDir(), "non_existent_file.yaml")
			}

			output, err := processCloneFile(filePath)

			if (err != nil) != test.expectError {
				t.Errorf("Expected error: %v, got: %v", test.expectError, err)
				return
			}

			if !test.expectError {
				if output == nil {
					t.Error("Expected non-nil output")
					return
				}

				if len(output) != len(test.expectedOutput) {
					t.Errorf("Expected %d forges, got %d", len(test.expectedOutput), len(output))
					return
				}

				for forgeKey, expectedConfig := range test.expectedOutput {
					actualConfig, exists := output[forgeKey]
					if !exists {
						t.Errorf("Expected forge '%s' not found in output", forgeKey)
						continue
					}
					if !equalConfigs(actualConfig, expectedConfig) {
						t.Errorf("For forge '%s':\nexpected: %+v\ngot:      %+v", forgeKey, expectedConfig, actualConfig)
					}
				}
			}
		})
	}
}

func TestRetrieveReposUrlFromUser(t *testing.T) {
	tests := []struct {
		name             string
		forge            string
		user             string
		instanceUrl      string
		expectedUrl      string
		expectedDir      string
	}{
		{
			name:        "GitHub user",
			forge:       "github",
			user:        "torvalds",
			instanceUrl: "",
			expectedUrl: "https://api.github.com/users/torvalds/repos?per_page=100",
			expectedDir: "torvalds",
		},
		{
			name:        "GitLab user without instance URL",
			forge:       "gitlab",
			user:        "someuser",
			instanceUrl: "",
			expectedUrl: "https://gitlab.com/api/v4/users/someuser/projects?per_page=100",
			expectedDir: "someuser",
		},
		{
			name:        "GitLab user with instance URL",
			forge:       "gitlab",
			user:        "someuser",
			instanceUrl: "https://gitlab.example.com",
			expectedUrl: "https://gitlab.example.com/api/v4/users/someuser/projects?per_page=100",
			expectedDir: "someuser",
		},
		{
			name:        "Gitea user without instance URL",
			forge:       "gitea",
			user:        "giteauser",
			instanceUrl: "",
			expectedUrl: "https://gitea.com/api/v1/users/giteauser/repos?per_page=100",
			expectedDir: "giteauser",
		},
		{
			name:        "Gitea user with instance URL",
			forge:       "gitea",
			user:        "giteauser",
			instanceUrl: "https://gitea.example.com/",
			expectedUrl: "https://gitea.example.com/api/v1/users/giteauser/repos?per_page=100",
			expectedDir: "giteauser",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			url, dir, _ := retrieveReposUrlFromUser(test.forge, test.user, test.instanceUrl)

			if url != test.expectedUrl {
				t.Errorf("Expected URL '%s', got '%s'", test.expectedUrl, url)
			}
			if dir != test.expectedDir {
				t.Errorf("Expected dir '%s', got '%s'", test.expectedDir, dir)
			}
		})
	}
}

func TestRetrieveReposUrlFromOrganisation(t *testing.T) {
	tests := []struct {
		name        string
		forge       string
		org         string
		instanceUrl string
		expectedUrl string
		expectedDir string
	}{
		{
			name:        "GitHub organisation",
			forge:       "github",
			org:         "golang",
			instanceUrl: "",
			expectedUrl: "https://api.github.com/orgs/golang/repos?per_page=100",
			expectedDir: "golang",
		},
		{
			name:        "GitLab organisation without instance URL",
			forge:       "gitlab",
			org:         "mygroup",
			instanceUrl: "",
			expectedUrl: "https://gitlab.com/api/v4/groups/mygroup/projects?per_page=100&include_subgroups=true",
			expectedDir: "mygroup",
		},
		{
			name:        "GitLab organisation with instance URL",
			forge:       "gitlab",
			org:         "mygroup",
			instanceUrl: "https://gitlab.example.com",
			expectedUrl: "https://gitlab.example.com/api/v4/groups/mygroup/projects?per_page=100&include_subgroups=true",
			expectedDir: "mygroup",
		},
		{
			name:        "Gitea organisation without instance URL",
			forge:       "gitea",
			org:         "myorg",
			instanceUrl: "",
			expectedUrl: "https://gitea.com/api/v1/orgs/myorg/repos?per_page=100",
			expectedDir: "myorg",
		},
		{
			name:        "Gitea organisation with instance URL",
			forge:       "gitea",
			org:         "myorg",
			instanceUrl: "https://gitea.example.com/",
			expectedUrl: "https://gitea.example.com/api/v1/orgs/myorg/repos?per_page=100",
			expectedDir: "myorg",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			url, dir, _ := retrieveReposUrlFromOrganisation(test.forge, test.org, test.instanceUrl)

			if url != test.expectedUrl {
				t.Errorf("Expected URL '%s', got '%s'", test.expectedUrl, url)
			}
			if dir != test.expectedDir {
				t.Errorf("Expected dir '%s', got '%s'", test.expectedDir, dir)
			}
		})
	}
}

func TestCollectRepositories_Generic_GitHub(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		token     string
		minCount  int
		expectErr bool
	}{
		{
			name:     "Valid public user with repos",
			url:      "https://api.github.com/users/torvalds/repos?per_page=100",
			token:    "",
			minCount: 1,
		},
		{
			name:     "Valid public user but no repos",
			url:      "https://api.github.com/users/bno123bno/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Valid public org with repos",
			url:      "https://api.github.com/orgs/golang/repos?per_page=100",
			token:    "",
			minCount: 1,
		},
		{
			name:     "Valid public org but no repos",
			url:      "https://api.github.com/orgs/Empty-Organisation/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Non-existent user",
			url:      "https://api.github.com/users/thisuserdefinitelydoesnotexist99999/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Non-existent organisation",
			url:      "https://api.github.com/users/thisorganisationdefinitelydoesnotexist99999/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Invalid URL",
			url:      "https://api.github.com/invalid/endpoint?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Invalid token",
			url:      "https://api.github.com/users/sdobrau/repos?per_page=100",
			token:    "invalid",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "User with 200+ repositories. Make sure all repos are fetched",
			url:      "https://api.github.com/users/proppy/repos?per_page=100",
			token:    "",
			minCount: 287,
		},
	}

	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITHUB_TOKEN")
		}
		req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos, err := collectRepositories[GitHubRepository](tt.url, tt.token, header)

			if (err != nil) != tt.expectErr {
				t.Fatalf("Expected error=%v, got err=%v", tt.expectErr, err)
			}
			if !tt.expectErr && len(repos) < tt.minCount {
				t.Fatalf("Expected at least %d repos, got %d", tt.minCount, len(repos))
			}
		})
	}
}

func TestCollectRepositories_Generic_GitLab(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		token     string
		minCount  int
		expectErr bool
	}{
		{
			name:     "Valid public user with repos",
			url:      "https://gitlab.com/api/v4/users/strangerpr0gram/projects?per_page=100",
			token:    "",
			minCount: 1,
		},
		{
			name:     "Valid public user but no repos",
			url:      "https://gitlab.com/api/v4/users/immaroot/projects?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Valid public group with repos",
			url:      "https://gitlab.com/api/v4/groups/pkf-projects/projects?per_page=100&include_subgroups=true",
			token:    "",
			minCount: 1,
		},
		{
			name:     "Valid public group but no repos",
			url:      "https://gitlab.com/api/v4/groups/federated-library-system/projects?per_page=100&include_subgroups=true",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Non-existent user",
			url:      "https://gitlab.com/api/v4/users/thisuserdefinitelydoesnotexist99999/projects?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Non-existent organisation",
			url:      "https://gitlab.com/api/v4/users/thisorganisationdefinitelydoesnotexist99999/projects?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Invalid token",
			url:      "https://gitlab.com/api/v4/users/strangerpr0gram/projects?per_page=100",
			token:    "invalid",
			minCount: 0,
			expectErr: true,
		},
	}

	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITLAB_TOKEN")
		}
		req.Header.Set("PRIVATE-TOKEN", token)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos, err := collectRepositories[GitLabRepository](tt.url, tt.token, header)

			if (err != nil) != tt.expectErr {
				t.Fatalf("Expected error=%v, got err=%v", tt.expectErr, err)
			}
			if !tt.expectErr && len(repos) < tt.minCount {
				t.Fatalf("Expected at least %d repos, got %d", tt.minCount, len(repos))
			}
		})
	}
}

func TestCollectRepositories_Generic_Gitea(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		token     string
		minCount  int
		expectErr bool
	}{
		{
			name:     "Valid public user with repos",
			url:      "https://gitea.com/api/v1/users/gitea/repos?per_page=100",
			token:    "",
			minCount: 1,
		},
		{
			name:     "Valid public user but no repos",
			url:      "https://gitea.com/api/v1/users/gittower-test/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Valid public org with repos",
			url:      "https://gitea.com/api/v1/orgs/gitea/repos?per_page=100",
			token:    "",
			minCount: 1,
		},
		{
			name:     "Valid public org but no repos",
			url:      "https://gitea.com/api/v1/orgs/ftfy/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Non-existent user",
			url:      "https://gitea.com/api/v1/users/thisuserdefinitelydoesnotexist99999/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Non-existent organisation",
			url:      "https://gitea.com/api/v1/users/thisorganisationdefinitelydoesnotexist99999/repos?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Invalid URL",
			url:      "https://gitea.com/api/v1/invalid/endpoint?per_page=100",
			token:    "",
			minCount: 0,
			expectErr: true,
		},
		{
			name:     "Invalid token",
			url:      "https://gitea.com/api/v1/users/mayx/repos/per_page=100",
			token:    "invalid",
			minCount: 0,
			expectErr: true,
		},
	}

	header := func(req *http.Request, token string) {
		if token == "" {
			token = os.Getenv("GITEA_TOKEN")
		}
		req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos, err := collectRepositories[GiteaRepository](tt.url, tt.token, header)

			if (err != nil) != tt.expectErr {
				t.Fatalf("Expected error=%v, got err=%v", tt.expectErr, err)
			}
			if !tt.expectErr && len(repos) < tt.minCount {
				t.Fatalf("Expected at least %d repos, got %d", tt.minCount, len(repos))
			}
		})
	}
}

func TestRetrieveRepositoriesFromForgeUrl(t *testing.T) {
	tests := []struct {
		name        string
		forge       string
		user        string
		url         string
		token       string
		srhtToken   string
		minCount    int
		expectError bool
	}{
		{
			name:        "GitHub forge with valid user",
			forge:       "github",
			user:        "sdobrau",
			url:         "https://api.github.com/users/sdobrau/repos?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    1,
			expectError: false,
		},
		{
			name:        "GitLab forge with valid user",
			forge:       "gitlab",
			user:        "strangerpr0gram",
			url:         "https://gitlab.com/api/v4/users/strangerpr0gram/projects?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    1,
			expectError: false,
		},
		{
			name:        "Gitea forge with valid user",
			forge:       "gitea",
			user:        "mayx",
			url:         "https://gitea.com/api/v1/users/mayx/repos?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    1,
			expectError: false,
		},
		{
			name:        "GitHub forge with non-existent user",
			forge:       "github",
			user:        "thisuserdefinitelydoesnotexist99999",
			url:         "https://api.github.com/users/thisuserdefinitelydoesnotexist99999/repos?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    0,
			expectError: true,
		},
		{
			name:        "GitLab forge with non-existent user",
			forge:       "gitlab",
			user:        "thisuserdefinitelydoesnotexist99999",
			url:         "https://gitlab.com/api/v4/users/thisuserdefinitelydoesnotexist99999/projects?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    0,
			expectError: true,
		},
		{
			name:        "Gitea forge with non-existent user",
			forge:       "gitea",
			user:        "thisuserdefinitelydoesnotexist99999",
			url:         "https://gitea.com/api/v1/users/thisuserdefinitelydoesnotexist99999/repos?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    0,
			expectError: true,
		},
		{
			name:        "GitHub forge with non-existent organisation",
			forge:       "github",
			user:        "thisorganisationdefinitelydoesnotexist99999",
			url:         "https://api.github.com/orgs/thisuserdefinitelydoesnotexist99999/repos?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    0,
			expectError: true,
		},
		{
			name:        "GitLab forge with non-existent organisation",
			forge:       "gitlab",
			user:        "thisorganisationdefinitelydoesnotexist99999",
			url:         "https://gitlab.com/api/v4/users/thisorganisationdefinitelydoesnotexist99999/projects?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    0,
			expectError: true,
		},
		{
			name:        "Gitea forge with non-existent organisation",
			forge:       "gitea",
			user:        "thisorganisationdefinitelydoesnotexist99999",
			url:         "https://gitea.com/api/v1/orgs/thisorganisationdefinitelydoesnotexist99999/repos?per_page=100",
			token:       "",
			srhtToken:   "",
			minCount:    0,
			expectError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repos, err := retrieveRepositoriesFromForgeUrl(test.forge, test.user, test.url, test.token, test.srhtToken)

			if (err != nil) != test.expectError {
				t.Errorf("Expected error=%v, got err=%v", test.expectError, err)
			}
			if !test.expectError && len(repos) < test.minCount {
				t.Errorf("Expected at least %d repos, got %d", test.minCount, len(repos))
			}
		})
	}
}

func TestCloneOrPullRepositoryListt (t *testing.T) {
	tests := []struct {
		name         string
		repos        []Repository
		forge        string
		dirToAppend  string
		ignoreForks  bool
		starsGreater uint
		threads      uint
		expectedCloned int // -1 means skip count check
	}{
		{
			name: "Clone non-fork repos with sufficient stars",
			repos: []Repository{
				GitHubRepository{Name: "repo1", Url: "https://github.com/sdobrau/.emacs.d", Fork: false, StargazersCount: 0},
			},
			forge:        "github",
			dirToAppend:  "sdobrau",
			ignoreForks:  true,
			starsGreater: 0,
			threads:      20,
		},
		{
			name: "Skip forks when ignoreForks is true",
			repos: []Repository{
				GitHubRepository{Name: "forked-repo", Url: "https://github.com/sdobrau/go-git", Fork: true, StargazersCount: 0},
			},
			forge:        "github",
			dirToAppend:  "sdobrau",
			ignoreForks:  true,
			starsGreater: 0,
			threads:      20,
		},
		{
			name: "Skip repos below star threshold",
			repos: []Repository{
				GitHubRepository{Name: "low-stars", Url: "https://github.com/sdobrau/.emacs.d", Fork: false, StargazersCount: 0},
			},
			forge:        "github",
			dirToAppend:  "sdobrau",
			ignoreForks:  false,
			starsGreater: 10,
			threads:      20,
		},
		{
			name:         "Empty repository list",
			repos:        []Repository{},
			forge:        "github",
			dirToAppend:  "emptyuser",
			ignoreForks:  false,
			starsGreater: 0,
			threads:      20,
		},
		{
			name: "Clone all repositories and verify count for small user (sdobrau, 11 repositories)",
			repos: func() []Repository {
				repos, err := collectGitHubRepositories(
					"https://api.github.com/users/sdobrau/repos?per_page=100", "")
				if err != nil {
					return []Repository{}
				}
				var result []Repository
				for _, r := range repos {
					result = append(result, r)
				}
				return result
			}(),
			forge:          "github",
			dirToAppend:    "sdobrau",
			ignoreForks:    false,
			starsGreater:   0,
			threads:        20,
			expectedCloned: 11,
		},
		{
			name: "Clone all repositories and verify count for medium user (tj, 296 repositories)",
			repos: func() []Repository {
				repos, err := collectGitHubRepositories(
					"https://api.github.com/users/tj/repos?per_page=100", "")
				if err != nil {
					return []Repository{}
				}
				var result []Repository
				for _, r := range repos {
					result = append(result, r)
				}
				return result
			}(),
			forge:          "github",
			dirToAppend:    "tj",
			ignoreForks:    false,
			starsGreater:   0,
			threads:        20,
			expectedCloned: 296,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "processrepos-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			// clear when calling function exits
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"
			var wg sync.WaitGroup

			// should not panic
			cloneOrPullRepositoryList(test.repos, test.forge, dir, test.dirToAppend, test.ignoreForks, test.starsGreater, &wg, test.threads)

			if test.expectedCloned >= 0 {
				repoBaseDir := filepath.Join(dir, test.forge, test.dirToAppend)
				entries, err := os.ReadDir(repoBaseDir)
				if err != nil && test.expectedCloned > 0 {
					t.Fatalf("Failed to read repo dir: %v", err)
				}

				clonedCount := 0
				for _, entry := range entries {
					if entry.IsDir() {
						gitDir := filepath.Join(repoBaseDir, entry.Name(), ".git")
						if _, err := os.Stat(gitDir); err == nil {
							clonedCount++
						}
					}
				}

				if clonedCount < test.expectedCloned {
					t.Errorf("Expected at least %d cloned repos, got %d", test.expectedCloned, clonedCount)
				}
			}
		})
	}
}

func TestCloneRepository(t *testing.T) {
	tests := []struct {
		name        string
		repo        Repository
		expectClone bool
	}{
		{
			name:        "Clone a small valid repository",
			repo:        GitHubRepository{Name: "hello-world", Url: "https://github.com/octocat/Hello-World", Fork: false, StargazersCount: 0},
			expectClone: true,
		},
		{
			name:        "Clone a non-existent repository",
			repo:        GitHubRepository{Name: "nonexistent", Url: "https://github.com/thisuserdefinitelydoesnotexist99999/nonexistent", Fork: false, StargazersCount: 0},
			expectClone: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "clonerepo-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := filepath.Join(tmpDir, test.repo.GetName())
			cloneRepository(test.repo, dir)

			_, err = os.Stat(filepath.Join(dir, ".git"))
			cloned := err == nil

			if cloned != test.expectClone {
				t.Errorf("Expected clone=%v, got clone=%v", test.expectClone, cloned)
			}
		})
	}
}

func TestPullRepository(t *testing.T) {
	tests := []struct {
		name string
		repo Repository
	}{
		{
			name: "Pull an already cloned repository",
			repo: GitHubRepository{Name: "Hello-World", Url: "https://github.com/octocat/Hello-World", Fork: false, StargazersCount: 0},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "pullrepo-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := filepath.Join(tmpDir, test.repo.GetName())

			// clone first
			cloneRepository(test.repo, dir)

			_, err = os.Stat(filepath.Join(dir, ".git"))
			if err != nil {
				t.Fatalf("Clone failed, cannot test pull: %v", err)
			}

			// should not panic or error
			pullRepository(dir)
		})
	}
}

func TestCloneOrPullRepositoryAsync(t *testing.T) {
	tests := []struct {
		name         string
		repo         Repository
		preClone     bool
		expectDotGit bool
	}{
		{
			name:         "Async clone of a new repository",
			repo:         GitHubRepository{Name: "Hello-World", Url: "https://github.com/octocat/Hello-World", Fork: false, StargazersCount: 0},
			preClone:     false,
			expectDotGit: true,
		},
		{
			name:         "Async pull of an already cloned repository",
			repo:         GitHubRepository{Name: "Hello-World", Url: "https://github.com/octocat/Hello-World", Fork: false, StargazersCount: 0},
			preClone:     true,
			expectDotGit: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "asyncrepo-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := filepath.Join(tmpDir, test.repo.GetName())

			if test.preClone {
				cloneRepository(test.repo, dir)
				_, err = os.Stat(filepath.Join(dir, ".git"))
				if err != nil {
					t.Fatalf("Pre-clone failed: %v", err)
				}
			}

			var wg sync.WaitGroup
			cloneOrPullRepositoryAsync(dir, test.repo, &wg)
			wg.Wait()

			_, err = os.Stat(filepath.Join(dir, ".git"))
			hasDotGit := err == nil

			if hasDotGit != test.expectDotGit {
				t.Errorf("Expected .git=%v, got .git=%v", test.expectDotGit, hasDotGit)
			}
		})
	}
}
