package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// DependabotAlert is one open Dependabot alert as the read-only alert tool shows it.
//
// GitHub's alert does not report the exact resolved version a manifest locked to; it reports the
// vulnerable range the advisory matched (VulnerableVersionRange) and, when GitHub knows one, the
// first version that fixes it (PatchedVersion, empty when none exists yet).
type DependabotAlert struct {
	Number                 int
	State                  string
	PackageName            string
	Ecosystem              string
	ManifestPath           string
	Severity               string
	VulnerableVersionRange string
	PatchedVersion         string
	AdvisoryGHSAID         string
	AdvisorySummary        string
	URL                    string
	HTMLURL                string
}

type dependabotAlertJSON struct {
	Number     int    `json:"number"`
	State      string `json:"state"`
	Dependency struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		ManifestPath string `json:"manifest_path"`
	} `json:"dependency"`
	SecurityAdvisory struct {
		GHSAID  string `json:"ghsa_id"`
		Summary string `json:"summary"`
	} `json:"security_advisory"`
	SecurityVulnerability struct {
		Severity               string `json:"severity"`
		VulnerableVersionRange string `json:"vulnerable_version_range"`
		FirstPatchedVersion    *struct {
			Identifier string `json:"identifier"`
		} `json:"first_patched_version"`
	} `json:"security_vulnerability"`
	URL     string `json:"url"`
	HTMLURL string `json:"html_url"`
}

func (a dependabotAlertJSON) alert() DependabotAlert {
	out := DependabotAlert{
		Number: a.Number, State: a.State,
		PackageName: a.Dependency.Package.Name, Ecosystem: a.Dependency.Package.Ecosystem, ManifestPath: a.Dependency.ManifestPath,
		Severity: a.SecurityVulnerability.Severity, VulnerableVersionRange: a.SecurityVulnerability.VulnerableVersionRange,
		AdvisoryGHSAID: a.SecurityAdvisory.GHSAID, AdvisorySummary: a.SecurityAdvisory.Summary,
		URL: a.URL, HTMLURL: a.HTMLURL,
	}
	if a.SecurityVulnerability.FirstPatchedVersion != nil {
		out.PatchedVersion = a.SecurityVulnerability.FirstPatchedVersion.Identifier
	}
	return out
}

// ListDependabotAlerts lists the repository's open Dependabot alerts. A repository with none
// returns an empty slice, not an error.
func (a *App) ListDependabotAlerts(ctx context.Context, repo Repo) ([]DependabotAlert, error) {
	var all []DependabotAlert
	for page := 1; page <= listMaxPages; page++ {
		q := url.Values{"state": {"open"}, "per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)}}
		var got []dependabotAlertJSON
		path := repoPath(repo, "dependabot", "alerts") + "?" + q.Encode()
		if err := a.call(ctx, repo, Permissions{"vulnerability_alerts": Read}, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		if len(got) > listPerPage {
			return nil, ErrUnavailable
		}
		for _, g := range got {
			all = append(all, g.alert())
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}
