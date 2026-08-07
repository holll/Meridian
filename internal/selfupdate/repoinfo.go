package selfupdate

// RepoInfo is GitHub repository metadata shown in the About dialog.
type RepoInfo struct {
	HTMLURL     string `json:"html_url"`
	Description string `json:"description"`
	Stars       int    `json:"stars"`
	Forks       int    `json:"forks"`
	License     string `json:"license"`
	OpenIssues  int    `json:"open_issues"`
}
