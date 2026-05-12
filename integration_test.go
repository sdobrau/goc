//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// * Helper: build the binary once for all integration tests
var binaryPath string

func TestMain(m *testing.M) {
	// Build the binary
	tmpDir, err := os.MkdirTemp("", "goc-integration-*")
	if err != nil {
		panic(err)
	}
	binaryPath = filepath.Join(tmpDir, "goc")

	cmd := exec.Command("go", "build", "-o", binaryPath, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("Failed to build binary: " + err.Error())
	}

	// Run all tests
	exitCode := m.Run()

	// Cleanup
	os.RemoveAll(tmpDir)
	os.Exit(exitCode)
}

// * Helper: run the goc binary with args
func runGoc(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binaryPath, args...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		exitCode = -1
	}

	return stdout.String(), stderr.String(), exitCode
}

// * Helper: count cloned repos in a directory
func countClonedRepos(t *testing.T, dir string) int {
	t.Helper()
	count := 0

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			gitDir := filepath.Join(dir, entry.Name(), ".git")
			if _, err := os.Stat(gitDir); err == nil {
				count++
			}
		}
	}
	return count
}

// * Test: CLI flag validation
func TestIntegrationCLIFlags(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		expectExit   int
		stderrContains string
	}{
		{
			name:           "No arguments shows usage",
			args:           []string{},
			expectExit:     1,
			stderrContains: "usage",
		},
		{
			name:           "Missing forge flag",
			args:           []string{"-u", "torvalds"},
			expectExit:     1,
			stderrContains: "forge",
		},
		{
			name:           "Both user and org fails",
			args:           []string{"-f", "github", "-u", "torvalds", "-o", "golang"},
			expectExit:     1,
			stderrContains: "both",
		},
		{
			name:           "Clone file with user fails",
			args:           []string{"-f", "github", "-u", "torvalds", "-F", "config.yaml"},
			expectExit:     1,
			stderrContains: "",
		},
		{
			name:           "SourceHut without token fails",
			args:           []string{"-f", "sourcehut", "-u", "sircmpwn"},
			expectExit:     1,
			stderrContains: "token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, stderr, exitCode := runGoc(t, test.args...)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}
			if test.stderrContains != "" && !strings.Contains(strings.ToLower(stderr), strings.ToLower(test.stderrContains)) {
				t.Errorf("Expected stderr to contain '%s', got: %s", test.stderrContains, stderr)
			}
		})
	}
}

// * Test: Clone a small GitHub user
func TestIntegrationCloneGitHubUser(t *testing.T) {
	tests := []struct {
		name          string
		args          func(dir string) []string
		minCloned     int
		expectExit    int
		checkForge    string
		checkUser     string
	}{
		{
			name: "Clone small GitHub user (octocat)",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "octocat", "-d", dir}
			},
			minCloned:  8,
			expectExit: 0,
			checkForge: "github",
			checkUser:  "octocat",
		},
		{
			name: "Clone GitHub user ignoring forks",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "octocat", "-d", dir, "-x"}
			},
			minCloned:  6,
			expectExit: 0,
			checkForge: "github",
			checkUser:  "octocat",
		},
		{
			name: "Clone GitHub user with star threshold",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "octocat", "-d", dir, "-s", "3000"}
			},
			minCloned:  2,
			expectExit: 0,
			checkForge: "github",
			checkUser:  "octocat",
		},
		{
			name: "Clone small GitHub user",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "SleeplessByte", "-d", dir}
			},
			minCloned:  116,
			expectExit: 0,
			checkForge: "github",
			checkUser:  "SleeplessByte",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-gh-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"
			_, _, exitCode := runGoc(t, test.args(dir)...)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}

			repoDir := filepath.Join(dir, test.checkForge, test.checkUser)
			cloned := countClonedRepos(t, repoDir)
			if cloned < test.minCloned {
				t.Errorf("Expected at least %d cloned repos, got %d", test.minCloned, cloned)
			}
		})
	}
}

// * Test: Clone a GitHub organisation
func TestIntegrationCloneGitHubOrg(t *testing.T) {
	tests := []struct {
		name       string
		args       func(dir string) []string
		minCloned  int
		expectExit int
		checkForge string
		checkOrg   string
	}{
		{
			name: "Clone small GitHub organisation",
			args: func(dir string) []string {
				return []string{"-f", "github", "-o", "cli", "-d", dir}
			},
			minCloned:  10,
			expectExit: 0,
			checkForge: "github",
			checkOrg:   "cli",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-org-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"
			_, _, exitCode := runGoc(t, test.args(dir)...)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}

			repoDir := filepath.Join(dir, test.checkForge, test.checkOrg)
			cloned := countClonedRepos(t, repoDir)
			if cloned < test.minCloned {
				t.Errorf("Expected at least %d cloned repos, got %d", test.minCloned, cloned)
			}
		})
	}
}

// * Test: Clone GitLab user
func TestIntegrationCloneGitLabUser(t *testing.T) {
	tests := []struct {
		name       string
		args       func(dir string) []string
		minCloned  int
		expectExit int
		checkForge string
		checkUser  string
	}{
		{
			name: "Clone small GitLab user",
			args: func(dir string) []string {
				return []string{"-f", "gitlab", "-u", "gnachman", "-d", dir}
			},
			minCloned:  2,
			expectExit: 0,
			checkForge: "gitlab",
			checkUser:  "gnachman",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-gl-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"
			_, _, exitCode := runGoc(t, test.args(dir)...)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}

			repoDir := filepath.Join(dir, test.checkForge, test.checkUser)
			cloned := countClonedRepos(t, repoDir)
			if cloned < test.minCloned {
				t.Errorf("Expected at least %d cloned repos, got %d", test.minCloned, cloned)
			}
		})
	}
}

// * Test: Clone Gitea user
func TestIntegrationCloneGiteaUser(t *testing.T) {
	tests := []struct {
		name       string
		args       func(dir string) []string
		minCloned  int
		expectExit int
		checkForge string
		checkUser  string
	}{
		{
			name: "Clone small Gitea user",
			args: func(dir string) []string {
				return []string{"-f", "gitea", "-u", "gitea", "-d", dir}
			},
			minCloned:  53,
			expectExit: 0,
			checkForge: "gitea",
			checkUser:  "gitea",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-gt-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"
			_, _, exitCode := runGoc(t, test.args(dir)...)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}

			repoDir := filepath.Join(dir, test.checkForge, test.checkUser)
			cloned := countClonedRepos(t, repoDir)
			if cloned < test.minCloned {
				t.Errorf("Expected at least %d cloned repos, got %d", test.minCloned, cloned)
			}
		})
	}
}

// * Test: Clone from YAML config file
func TestIntegrationCloneFromFile(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		checks      []struct {
			forge     string
			user      string
			minCloned int
		}
		expectExit int
	}{
		{
			name: "Clone from YAML config with multiple forges",
			yamlContent: `
- forge: github
  users:
    - name: octocat
- forge: gitea
  users:
    - name: mayx
`,
			checks: []struct {
				forge     string
				user      string
				minCloned int
			}{
				{forge: "github", user: "octocat", minCloned: 8},
				{forge: "gitea", user: "mayx", minCloned: 1},
			},
			expectExit: 0,
		},
		{
			name: "Clone from YAML with multiple users",
			yamlContent: `
- forge: github
  users:
    - name: octocat
    - name: tj
      starsGreater: 18000
`,
			checks: []struct {
				forge     string
				user      string
				minCloned int
			}{
				{forge: "github", user: "octocat", minCloned: 8},
				{forge: "github", user: "tj", minCloned: 3},
			},
			expectExit: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-file-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			// Write YAML config
			configPath := filepath.Join(tmpDir, "config.yaml")
			err = os.WriteFile(configPath, []byte(test.yamlContent), 0644)
			if err != nil {
				t.Fatalf("Failed to write config file: %v", err)
			}

			dir := tmpDir + "/repos/"
			os.MkdirAll(dir, 0755)

			_, _, exitCode := runGoc(t, "-F", configPath, "-d", dir)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}

			for _, check := range test.checks {
				repoDir := filepath.Join(dir, check.forge, check.user)
				cloned := countClonedRepos(t, repoDir)
				if cloned < check.minCloned {
					t.Errorf("For %s/%s: expected at least %d cloned repos, got %d",
						check.forge, check.user, check.minCloned, cloned)
				}
			}
		})
	}
}

// * Test: Re-running clones should pull instead of fail
func TestIntegrationIdempotency(t *testing.T) {
	tests := []struct {
		name string
		args func(dir string) []string
		checkForge string
		checkUser  string
	}{
		{
			name: "Running twice should succeed (clone then pull)",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "octocat", "-d", dir}
			},
			checkForge: "github",
			checkUser:  "octocat",
		},
	}
	
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-idem-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"

			// First run: clone
			_, _, exitCode1 := runGoc(t, test.args(dir)...)
			if exitCode1 != 0 {
				t.Fatalf("First run failed with exit code %d", exitCode1)
			}

			repoDir := filepath.Join(dir, test.checkForge, test.checkUser)
			countAfterClone := countClonedRepos(t, repoDir)
			if countAfterClone == 0 {
				t.Fatal("First run cloned zero repos")
			}

			// Second run: should pull, not fail
			_, _, exitCode2 := runGoc(t, test.args(dir)...)
			if exitCode2 != 0 {
				t.Fatalf("Second run failed with exit code %d", exitCode2)
			}

			countAfterPull := countClonedRepos(t, repoDir)
			if countAfterPull != countAfterClone {
				t.Errorf("Repo count changed after pull: clone=%d, pull=%d", countAfterClone, countAfterPull)
			}
		})
	}
}

// * Test: Non-existent user should fail gracefully
func TestIntegrationNonExistentUser(t *testing.T) {
	tests := []struct {
		name       string
		args       func(dir string) []string
		expectExit int
	}{
		{
			name: "GitHub non-existent user",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "thisuserdefinitelydoesnotexist99999", "-d", dir}
			},
			expectExit: 1,
		},
		{
			name: "GitLab non-existent user",
			args: func(dir string) []string {
				return []string{"-f", "gitlab", "-u", "thisuserdefinitelydoesnotexist99999", "-d", dir}
			},
			expectExit: 1,
		},
		{
			name: "Gitea non-existent user",
			args: func(dir string) []string {
				return []string{"-f", "gitea", "-u", "thisuserdefinitelydoesnotexist99999", "-d", dir}
			},
			expectExit: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-nouser-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"
			_, _, exitCode := runGoc(t, test.args(dir)...)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}
		})
	}
}

// * Test: Thread limiting works
func TestIntegrationThreadLimiting(t *testing.T) {
	tests := []struct {
		name       string
		args       func(dir string) []string
		minCloned  int
		checkForge string
		checkUser  string
	}{
		{
			name: "Clone with low goroutine limit",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "octocat", "-d", dir, "-t", "2"}
			},
			minCloned:  8,
			checkForge: "github",
			checkUser:  "octocat",
		},
		{
			name: "Clone with high goroutine limit",
			args: func(dir string) []string {
				return []string{"-f", "github", "-u", "octocat", "-d", dir, "-t", "50"}
			},
			minCloned:  8,
			checkForge: "github",
			checkUser:  "octocat",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "integration-threads-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			dir := tmpDir + "/"
			_, _, exitCode := runGoc(t, test.args(dir)...)

			if exitCode != 0 {
				t.Fatalf("Expected exit code 0, got %d", exitCode)
			}

			repoDir := filepath.Join(dir, test.checkForge, test.checkUser)
			cloned := countClonedRepos(t, repoDir)
			if cloned < test.minCloned {
				t.Errorf("Expected at least %d cloned repos, got %d", test.minCloned, cloned)
			}
		})
	}
}

// * Test: Invalid directory
func TestIntegrationInvalidDirectory(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		expectExit int
	}{
		{
			name:       "Non-existent directory",
			args:       []string{"-f", "github", "-u", "octocat", "-d", "/nonexistent/path/"},
			expectExit: 1,
		},
		{
			name:       "Non-writable directory",
			args:       []string{"-f", "github", "-u", "octocat", "-d", "/root/"},
			expectExit: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, exitCode := runGoc(t, test.args...)

			if exitCode != test.expectExit {
				t.Errorf("Expected exit code %d, got %d", test.expectExit, exitCode)
			}
		})
	}
}
