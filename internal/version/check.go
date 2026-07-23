// Package version checks for newer engram releases on GitHub.
package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	repoOwner = "relytcloud"
	repoName  = "engram"
)

var (
	checkTimeout           = 2 * time.Second
	githubLatestReleaseURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	httpClient             = http.DefaultClient
)

type CheckStatus string

const (
	StatusUpToDate        CheckStatus = "up_to_date"
	StatusUpdateAvailable CheckStatus = "update_available"
	StatusCheckFailed     CheckStatus = "check_failed"
)

type CheckResult struct {
	Status  CheckStatus
	Message string
}

// githubRelease is the subset of the GitHub releases API we care about.
type githubRelease struct {
	TagName string `json:"tag_name"`
}

// CheckLatest compares the running version against the latest GitHub release.
// It distinguishes between up-to-date, update available, and check failures.
func CheckLatest(current string) CheckResult {
	switch current {
	case "":
		return checkFailed("Could not check for updates: current version is unknown.")
	case "dev":
		return checkFailed("Could not check for updates: development builds do not map to a release version.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubLatestReleaseURL, nil)
	if err != nil {
		return checkFailed("Could not check for updates: could not create the GitHub request.")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := githubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return checkFailed("Could not check for updates: GitHub took too long to respond.")
		}
		return checkFailed(fmt.Sprintf("Could not check for updates: %v.", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return checkFailed(nonOKStatusMessage(resp.Status))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return checkFailed("Could not check for updates: could not read the GitHub response.")
	}

	latest := normalizeVersion(release.TagName)
	running := normalizeVersion(current)

	if latest == "" {
		return checkFailed("Could not check for updates: GitHub did not return a release version.")
	}

	if latest == running {
		return CheckResult{Status: StatusUpToDate}
	}

	if !isNewer(latest, running) {
		return CheckResult{Status: StatusUpToDate}
	}

	return CheckResult{
		Status: StatusUpdateAvailable,
		Message: fmt.Sprintf(
			"Update available: %s -> %s\nTo update:\n%s",
			running, latest, updateInstructions(),
		),
	}
}

// normalizeVersion strips a leading "v" prefix.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// isNewer returns true if latest > current using simple semver comparison.
func isNewer(latest, current string) bool {
	latestParts := splitVersion(latest)
	currentParts := splitVersion(current)

	for i := 0; i < 3; i++ {
		if latestParts[i] > currentParts[i] {
			return true
		}
		if latestParts[i] < currentParts[i] {
			return false
		}
	}
	return false
}

// splitVersion splits "1.8.1" into [1, 8, 1]. Returns [0,0,0] on parse failure.
func splitVersion(v string) [3]int {
	var parts [3]int
	segments := strings.SplitN(v, ".", 3)
	for i, s := range segments {
		if i >= 3 {
			break
		}
		for _, c := range s {
			if c >= '0' && c <= '9' {
				parts[i] = parts[i]*10 + int(c-'0')
			} else {
				break
			}
		}
	}
	return parts
}

// updateInstructions returns how to upgrade. This fork distributes only via
// GitHub Release archives (no Homebrew tap; the module path still declares the
// upstream owner, so `go install` would fetch upstream — neither is offered
// here), so we point users at the fork's releases page for every platform.
func updateInstructions() string {
	return fmt.Sprintf("  Download the latest release:\n  https://github.com/%s/%s/releases/latest", repoOwner, repoName)
}

func githubToken() string {
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

func nonOKStatusMessage(status string) string {
	msg := fmt.Sprintf("Could not check for updates: GitHub API returned %s.", status)
	if strings.HasPrefix(status, "401") || strings.HasPrefix(status, "403") {
		msg += " Set GH_TOKEN or GITHUB_TOKEN to reduce rate limits."
	}
	return msg
}

func checkFailed(message string) CheckResult {
	return CheckResult{Status: StatusCheckFailed, Message: message}
}
