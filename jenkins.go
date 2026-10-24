package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var errEmptyConfig = errors.New("config.xml has no root element")

// apiError carries the HTTP status of a failed Jenkins request so callers can
// branch on it (e.g. treat 404 as "no such resource") without string matching.
type apiError struct {
	status  int
	message string
	body    string
}

func (e *apiError) Error() string { return e.message }

func isStatus(err error, status int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == status
}

const (
	requestTimeout    = 30 * time.Second
	logRequestTimeout = 90 * time.Second
)

var knownFolderClasses = map[string]bool{
	"com.cloudbees.hudson.plugins.folder.Folder":                            true,
	"org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject": true,
	"jenkins.branch.OrganizationFolder":                                     true,
}

// ── API response types ───────────────────────────────────────────────────────

type buildSummary struct {
	Number            int             `json:"number"`
	URL               string          `json:"url"`
	Result            *string         `json:"result"`
	Building          bool            `json:"building"`
	Timestamp         int64           `json:"timestamp"`
	Duration          int64           `json:"duration"`
	EstimatedDuration int64           `json:"estimatedDuration"`
	DisplayName       string          `json:"displayName"`
	Actions           []buildAction   `json:"actions"`
	Artifacts         []buildArtifact `json:"artifacts"`
	ChangeSet         *changeSet      `json:"changeSet"`
	ChangeSets        []changeSet     `json:"changeSets"`
}

type buildAction struct {
	Class             string `json:"_class"`
	LastBuiltRevision *struct {
		SHA1   string `json:"SHA1"`
		Branch []struct {
			SHA1 string `json:"SHA1"`
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"lastBuiltRevision"`
	BuildsByBranchName map[string]struct {
		Revision *struct {
			SHA1 string `json:"SHA1"`
		} `json:"revision"`
	} `json:"buildsByBranchName"`
	Causes []struct {
		ShortDescription string `json:"shortDescription"`
	} `json:"causes"`
	Parameters []struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	} `json:"parameters"`
}

type buildArtifact struct {
	FileName     string `json:"fileName"`
	RelativePath string `json:"relativePath"`
	DisplayPath  string `json:"displayPath"`
}

type changeSet struct {
	Items []changeSetItem `json:"items"`
}

type changeSetItem struct {
	CommitID string `json:"commitId"`
	Author   struct {
		FullName string `json:"fullName"`
	} `json:"author"`
	Msg  string `json:"msg"`
	Date string `json:"date"`
}

type jobInfo struct {
	Class       string        `json:"_class"`
	Name        string        `json:"name"`
	FullName    string        `json:"fullName"`
	URL         string        `json:"url"`
	Color       string        `json:"color"`
	Description string        `json:"description"`
	LastBuild   *buildRefJSON `json:"lastBuild"`
}

type buildRefJSON struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type queueItem struct {
	ID           int    `json:"id"`
	URL          string `json:"url"`
	Why          string `json:"why"`
	InQueueSince int64  `json:"inQueueSince"`
	Task         struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"task"`
}

type pipelineDescribe struct {
	Stages []pipelineStage `json:"stages"`
}

type pipelineStage struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	DurationMillis int64  `json:"durationMillis"`
	Error          *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type pipelineStageDetails struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	StageFlowNodes []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"stageFlowNodes"`
}

type pipelineLogPayload struct {
	Text string `json:"text"`
}

type testReport struct {
	FailCount int         `json:"failCount"`
	SkipCount int         `json:"skipCount"`
	PassCount int         `json:"passCount"`
	Suites    []testSuite `json:"suites"`
}

type testSuite struct {
	Name  string     `json:"name"`
	Cases []testCase `json:"cases"`
}

type testCase struct {
	ClassName       string  `json:"className"`
	Name            string  `json:"name"`
	Status          string  `json:"status"`
	Duration        float64 `json:"duration"`
	ErrorDetails    string  `json:"errorDetails"`
	ErrorStackTrace string  `json:"errorStackTrace"`
}

// ── Client ───────────────────────────────────────────────────────────────────

// JenkinsClient is a thin authenticated HTTP client for the Jenkins REST API.
type JenkinsClient struct {
	baseURL           string
	authHeader        string
	repoJobMap        map[string][]string
	notifyCommitToken string
	http              *http.Client

	mu         sync.Mutex // guards the caches below (worker pool runs handlers concurrently)
	crumbCache *crumb
	crumbTried bool
}

type crumb struct {
	headerName string
	value      string
}

func NewJenkinsClient(cfg Config) *JenkinsClient {
	auth := base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + cfg.Token))
	return &JenkinsClient{
		baseURL:           strings.TrimRight(cfg.URL, "/"),
		authHeader:        "Basic " + auth,
		repoJobMap:        cfg.RepoJobMap,
		notifyCommitToken: cfg.NotifyCommitToken,
		http:              &http.Client{},
	}
}

func pathWithoutQuery(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}

func (c *JenkinsClient) send(ctx context.Context, method, path, accept string, body io.Reader, header http.Header) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: building the request: %w", method, pathWithoutQuery(path), err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	res, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, nil, fmt.Errorf("%s %s: %w", method, pathWithoutQuery(path), err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: reading the response: %w", method, pathWithoutQuery(path), err)
	}
	return res, data, nil
}

func statusError(status int, method, path string, body []byte) *apiError {
	return &apiError{status: status, message: formatJenkinsError(status, method, path, string(body)), body: string(body)}
}

func (c *JenkinsClient) requestJSON(ctx context.Context, path string, dest any, timeout time.Duration) error {
	body, err := c.requestRaw(ctx, "GET", path, "application/json", timeout)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("GET %s: Jenkins returned an empty body where JSON was expected", pathWithoutQuery(path))
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("GET %s: decoding the JSON response: %w", pathWithoutQuery(path), err)
	}
	return nil
}

func (c *JenkinsClient) requestRaw(ctx context.Context, method, path, accept string, timeout time.Duration) ([]byte, error) {
	if timeout == 0 {
		timeout = requestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, body, err := c.send(ctx, method, path, accept, nil, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, statusError(res.StatusCode, method, path, body)
	}
	return body, nil
}

const connectionCheckTimeout = 5 * time.Second

func (c *JenkinsClient) checkConnection(ctx context.Context) error {
	var me struct {
		ID string `json:"id"`
	}
	if err := c.requestJSON(ctx, "/me/api/json", &me, connectionCheckTimeout); err != nil {
		return fmt.Errorf("checking the Jenkins connection to %s: %w", c.baseURL, err)
	}
	if me.ID == "" {
		return fmt.Errorf("checking the Jenkins connection to %s: /me/api/json returned no user id", c.baseURL)
	}
	return nil
}

func (c *JenkinsClient) fetchCrumb(ctx context.Context) (*crumb, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.crumbTried {
		return c.crumbCache, nil
	}
	var data struct {
		CrumbRequestField string `json:"crumbRequestField"`
		Crumb             string `json:"crumb"`
	}
	err := c.requestJSON(ctx, "/crumbIssuer/api/json", &data, 0)
	switch {
	case isStatus(err, http.StatusNotFound):
		c.crumbTried = true
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("fetching a CSRF crumb: %w", err)
	case data.Crumb == "" || data.CrumbRequestField == "":
		return nil, errors.New("fetching a CSRF crumb: /crumbIssuer/api/json returned no crumb or crumbRequestField")
	}
	c.crumbTried = true
	c.crumbCache = &crumb{headerName: data.CrumbRequestField, value: data.Crumb}
	return c.crumbCache, nil
}

func (c *JenkinsClient) post(ctx context.Context, path string, form url.Values, xml string) (string, error) {
	cr, err := c.fetchCrumb(ctx)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", pathWithoutQuery(path), err)
	}
	header := http.Header{}
	var body io.Reader
	switch {
	case xml != "":
		body = strings.NewReader(xml)
		header.Set("Content-Type", "application/xml")
	case len(form) > 0:
		body = strings.NewReader(form.Encode())
		header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cr != nil {
		header.Set(cr.headerName, cr.value)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, data, err := c.send(ctx, "POST", path, "application/json", body, header)
	if err != nil {
		return "", err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", statusError(res.StatusCode, "POST", path, data)
	}
	return res.Header.Get("Location"), nil
}

func (c *JenkinsClient) resolveJobsForRemote(remote string) []string {
	if remote == "" {
		return nil
	}
	lc := strings.ToLower(remote)
	seen := map[string]bool{}
	var matches []string
	for _, needle := range slices.Sorted(maps.Keys(c.repoJobMap)) {
		if strings.Contains(lc, needle) {
			for _, j := range c.repoJobMap[needle] {
				if !seen[j] {
					seen[j] = true
					matches = append(matches, j)
				}
			}
		}
	}
	return matches
}

func (c *JenkinsClient) hasRepoMapping() bool { return len(c.repoJobMap) > 0 }

// ── API getters ───────────────────────────────────────────────────────────────

func (c *JenkinsClient) getBuild(ctx context.Context, jobPath string, ref string) (*buildSummary, error) {
	segs := jobPathToAPISegments(jobPath)
	tree := "number,url,result,building,timestamp,duration,estimatedDuration,displayName,fullDisplayName,description,artifacts[fileName,relativePath,displayPath],actions[lastBuiltRevision[SHA1,branch[SHA1,name]],buildsByBranchName[*[revision[SHA1]]],causes[shortDescription,userId,userName,upstreamProject,upstreamBuild],parameters[name,value]],changeSets[items[commitId,author[fullName],msg,date]]"
	var b buildSummary
	if err := c.requestJSON(ctx, fmt.Sprintf("/%s/%s/api/json?tree=%s", segs, ref, url.QueryEscape(tree)), &b, 0); err != nil {
		return nil, err
	}
	return &b, nil
}

const maxBuildsPerRequest = 200

func (c *JenkinsClient) listBuilds(ctx context.Context, jobPath string, limit int) ([]buildSummary, error) {
	segs := jobPathToAPISegments(jobPath)
	tree := fmt.Sprintf("builds[number,url,result,building,timestamp,duration,displayName,actions[lastBuiltRevision[SHA1,branch[name]],causes[shortDescription]]]{0,%d}", limit)
	var res struct {
		Builds []buildSummary `json:"builds"`
	}
	if err := c.requestJSON(ctx, fmt.Sprintf("/%s/api/json?tree=%s", segs, url.QueryEscape(tree)), &res, 0); err != nil {
		return nil, err
	}
	return res.Builds, nil
}

func (c *JenkinsClient) findBuildBySha(ctx context.Context, jobPath, sha string, scanLimit int) (*buildSummary, int, error) {
	builds, err := c.listBuilds(ctx, jobPath, scanLimit)
	if err != nil {
		return nil, 0, err
	}
	for i := range builds {
		if cand := extractCommit(builds[i].Actions); cand != "" && shaPrefixMatches(sha, cand) {
			return &builds[i], i + 1, nil
		}
	}
	return nil, len(builds), nil
}

func shaNotFoundMessage(jobPath, sha string, scanned, scanLimit int) string {
	if scanned < scanLimit {
		return fmt.Sprintf("No build of %q matches SHA %s. Scanned all %d build(s) the job still retains (scanLimit %d), so any older build for this commit has been discarded by the job's build retention.", jobPath, sha, scanned, scanLimit)
	}
	if scanLimit < maxBuildsPerRequest {
		return fmt.Sprintf("No build of %q matches SHA %s in the %d most recent build(s) scanned (scanLimit %d). The job retains more builds than that: increase scanLimit (max %d), or pass buildNumber explicitly.", jobPath, sha, scanned, scanLimit, maxBuildsPerRequest)
	}
	return fmt.Sprintf("No build of %q matches SHA %s in the %d most recent build(s) scanned (scanLimit %d, the maximum). The job retains more builds than that: pass buildNumber explicitly.", jobPath, sha, scanned, scanLimit)
}

func (c *JenkinsClient) getConsoleLog(ctx context.Context, jobPath string, buildNumber int, tailChars int) (string, error) {
	segs := jobPathToAPISegments(jobPath)
	path := fmt.Sprintf("/%s/%d/consoleText", segs, buildNumber)

	// Full log requested: download it all.
	if tailChars <= 0 {
		body, err := c.requestRaw(ctx, "GET", path, "text/plain", logRequestTimeout)
		if err != nil {
			return "", err
		}
		return string(body), nil
	}

	// Tail requested: ask for just the last tailChars bytes via an HTTP Range
	// request so multi-megabyte logs aren't pulled into memory whole. Servers
	// that ignore Range (200) or reject it (416) fall back to a full download.
	r, err := c.getRange(ctx, path, tailChars)
	if err != nil {
		return "", err
	}
	if r.status == http.StatusPartialContent {
		s := trimPartialRunePrefix(string(r.body))
		switch {
		case r.total == unknownTotal:
			return fmt.Sprintf("... (truncated, last %d bytes; Jenkins did not report the full size)\n", len(s)) + s, nil
		case r.total > int64(len(s)):
			return fmt.Sprintf("... (truncated, last %d of %d bytes)\n", len(s), r.total) + s, nil
		}
		return s, nil
	}
	if r.status == http.StatusOK {
		s := string(r.body)
		if len(s) > tailChars {
			return fmt.Sprintf("... (truncated, last %d of %d chars)\n", tailChars, len(s)) + s[len(s)-tailChars:], nil
		}
		return s, nil
	}
	// 416 Range Not Satisfiable (log smaller than the window) — fetch it whole.
	full, err := c.requestRaw(ctx, "GET", path, "text/plain", logRequestTimeout)
	if err != nil {
		return "", err
	}
	return string(full), nil
}

type rangeResult struct {
	body   []byte
	status int
	total  int64
}

const unknownTotal = -1

func (c *JenkinsClient) getRange(ctx context.Context, path string, lastN int) (rangeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, logRequestTimeout)
	defer cancel()
	res, body, err := c.send(ctx, "GET", path, "text/plain", nil, http.Header{"Range": {fmt.Sprintf("bytes=-%d", lastN)}})
	if err != nil {
		return rangeResult{}, err
	}
	switch res.StatusCode {
	case http.StatusOK:
		return rangeResult{body: body, status: res.StatusCode, total: int64(len(body))}, nil
	case http.StatusPartialContent:
		total, err := parseContentRangeTotal(res.Header.Get("Content-Range"))
		if err != nil {
			return rangeResult{}, fmt.Errorf("GET %s: %w", path, err)
		}
		return rangeResult{body: body, status: res.StatusCode, total: total}, nil
	case http.StatusRequestedRangeNotSatisfiable:
		return rangeResult{status: res.StatusCode}, nil
	default:
		return rangeResult{}, statusError(res.StatusCode, "GET", path, body)
	}
}

func (c *JenkinsClient) getPipelineStages(ctx context.Context, jobPath string, buildNumber int) (*pipelineDescribe, error) {
	segs := jobPathToAPISegments(jobPath)
	var d pipelineDescribe
	if err := c.requestJSON(ctx, fmt.Sprintf("/%s/%d/wfapi/describe", segs, buildNumber), &d, 0); err != nil {
		if isStatus(err, 404) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func (c *JenkinsClient) getPipelineStageDetails(ctx context.Context, jobPath string, buildNumber int, stageID string) (*pipelineStageDetails, error) {
	segs := jobPathToAPISegments(jobPath)
	var d pipelineStageDetails
	if err := c.requestJSON(ctx, fmt.Sprintf("/%s/%d/execution/node/%s/wfapi/describe", segs, buildNumber, url.PathEscape(stageID)), &d, 0); err != nil {
		if isStatus(err, 404) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func (c *JenkinsClient) getPipelineNodeLog(ctx context.Context, jobPath string, buildNumber int, nodeID string) (string, bool, error) {
	segs := jobPathToAPISegments(jobPath)
	var p pipelineLogPayload
	if err := c.requestJSON(ctx, fmt.Sprintf("/%s/%d/execution/node/%s/wfapi/log", segs, buildNumber, url.PathEscape(nodeID)), &p, logRequestTimeout); err != nil {
		if isStatus(err, 404) {
			return "", false, nil
		}
		return "", false, err
	}
	return p.Text, true, nil
}

func (c *JenkinsClient) getTestReport(ctx context.Context, jobPath string, buildNumber int) (*testReport, error) {
	segs := jobPathToAPISegments(jobPath)
	tree := "failCount,skipCount,passCount,duration,suites[name,duration,cases[className,name,status,duration,errorDetails,errorStackTrace]]"
	var r testReport
	if err := c.requestJSON(ctx, fmt.Sprintf("/%s/%d/testReport/api/json?tree=%s", segs, buildNumber, url.QueryEscape(tree)), &r, 0); err != nil {
		if isStatus(err, 404) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func (c *JenkinsClient) getQueue(ctx context.Context) ([]queueItem, error) {
	var res struct {
		Items []queueItem `json:"items"`
	}
	if err := c.requestJSON(ctx, "/queue/api/json?tree=items[id,url,why,inQueueSince,task[name,url]]", &res, 0); err != nil {
		return nil, err
	}
	return res.Items, nil
}

func (c *JenkinsClient) listJobs(ctx context.Context, folder string, limit int) ([]jobInfo, error) {
	base := ""
	if folder != "" {
		base = "/" + jobPathToAPISegments(folder)
	}
	tree := fmt.Sprintf("jobs[name,fullName,url,_class,color,buildable,lastBuild[number,url],lastCompletedBuild[number,url]]{0,%d}", limit)
	var res struct {
		Jobs []jobInfo `json:"jobs"`
	}
	if err := c.requestJSON(ctx, fmt.Sprintf("%s/api/json?tree=%s", base, url.QueryEscape(tree)), &res, 0); err != nil {
		return nil, err
	}
	return res.Jobs, nil
}

// ── Tool entry points (return formatted strings) ──────────────────────────────

type buildOverviewArgs struct {
	jobPath                                                              string
	buildNumber                                                          string
	sha                                                                  string
	scanLimit                                                            int
	includeStages, includeChangeSet, includeArtifacts, includeParameters bool
}

func (c *JenkinsClient) getBuildOverview(ctx context.Context, a buildOverviewArgs) (string, error) {
	var build *buildSummary
	var err error
	if a.sha != "" {
		var scanned int
		build, scanned, err = c.findBuildBySha(ctx, a.jobPath, a.sha, a.scanLimit)
		if err != nil {
			return "", err
		}
		if build == nil {
			return shaNotFoundMessage(a.jobPath, a.sha, scanned, a.scanLimit), nil
		}
	} else {
		build, err = c.getBuild(ctx, a.jobPath, a.buildNumber)
		if err != nil {
			return "", err
		}
	}

	var l lines
	l.add("Job:    %s", a.jobPath)
	dn := ""
	if build.DisplayName != "" && build.DisplayName != fmt.Sprintf("#%d", build.Number) {
		dn = fmt.Sprintf(" (%s)", build.DisplayName)
	}
	l.add("Build:  #%d%s", build.Number, dn)
	l.add("URL:    %s", build.URL)
	l.add("Result: %s", fmtResult(build.Result, build.Building))
	if build.Building && build.EstimatedDuration > 0 {
		elapsed := time.Now().UnixMilli() - build.Timestamp
		l.add("Progress: %s elapsed of ~%s estimated", fmtDuration(elapsed), fmtDuration(build.EstimatedDuration))
	} else {
		l.add("Duration: %s", fmtDuration(build.Duration))
	}
	l.add("Started: %s", fmtTimestamp(build.Timestamp))

	if sha := extractCommit(build.Actions); sha != "" {
		branch := extractBranch(build.Actions)
		if branch != "" {
			l.add("Commit:  %s (%s)", sha, branch)
		} else {
			l.add("Commit:  %s", sha)
		}
	}
	if cause := extractCause(build.Actions); cause != "" {
		l.add("Cause:   %s", cause)
	}

	if a.includeParameters {
		params, err := extractParameters(build.Actions)
		if err != nil {
			return "", err
		}
		if len(params) > 0 {
			l.blank()
			l.add("Parameters:")
			for _, p := range params {
				v := p.value
				if len(v) > 200 {
					v = v[:200] + "..."
				}
				l.add("  %s = %s", p.name, v)
			}
		}
	}

	if a.includeStages {
		stages, err := c.getPipelineStages(ctx, a.jobPath, build.Number)
		if err != nil {
			return "", fmt.Errorf("fetching the stages of build #%d: %w", build.Number, err)
		}
		if stages != nil && len(stages.Stages) > 0 {
			l.blank()
			l.add("Stages:")
			for _, s := range stages.Stages {
				l.add("  %-11s %7s  %s  [id=%s]", s.Status, fmtDuration(s.DurationMillis), s.Name, s.ID)
				if s.Error != nil && s.Error.Message != "" {
					l.add("    ↳ %s", truncLine(s.Error.Message, 200))
				}
			}
			for _, s := range stages.Stages {
				if s.Status == "FAILED" {
					l.blank()
					l.add("Tip: fetch just the failing stage's log with jenkins_get_log jobPath=%q buildNumber=%d stageId=%q.", a.jobPath, build.Number, s.ID)
					break
				}
			}
		}
	}

	if a.includeChangeSet {
		items := build.changeItems()
		if len(items) > 0 {
			l.blank()
			l.add("Changes (%d):", len(items))
			for i, it := range items {
				if i >= 10 {
					l.add("  ... and %d more", len(items)-10)
					break
				}
				short := "????????"
				if len(it.CommitID) >= 8 {
					short = it.CommitID[:8]
				}
				author := it.Author.FullName
				if author == "" {
					author = "unknown"
				}
				l.add("  %s  %s: %s", short, author, truncLine(it.Msg, 100))
			}
		}
	}

	if a.includeArtifacts && len(build.Artifacts) > 0 {
		l.blank()
		l.add("Artifacts (%d):", len(build.Artifacts))
		for i, art := range build.Artifacts {
			if i >= 20 {
				l.add("  ... and %d more", len(build.Artifacts)-20)
				break
			}
			l.add("  %sartifact/%s", build.URL, art.RelativePath)
		}
	}

	if r := deref(build.Result); r == "FAILURE" || r == "UNSTABLE" {
		l.blank()
		l.add("Tips:")
		l.add("  • jenkins_get_tests jobPath=%q buildNumber=%d  (failed test names + stack traces)", a.jobPath, build.Number)
		l.add("  • jenkins_get_log   jobPath=%q buildNumber=%d  (console log; tailChars caps size)", a.jobPath, build.Number)
	}

	return l.String(), nil
}

func (c *JenkinsClient) getLog(ctx context.Context, jobPath string, buildNumber int, stageID string, tailChars int, full bool) (string, error) {
	tail := tailChars
	if full {
		tail = 0
	}

	if stageID != "" {
		details, err := c.getPipelineStageDetails(ctx, jobPath, buildNumber, stageID)
		if err != nil {
			return "", err
		}
		if details == nil {
			return fmt.Sprintf("No pipeline stage with id %q found for build #%d. Use jenkins_get_build to list stage ids.", stageID, buildNumber), nil
		}
		if len(details.StageFlowNodes) == 0 {
			stageLog, ok, err := c.getPipelineNodeLog(ctx, jobPath, buildNumber, stageID)
			if err != nil {
				return "", err
			}
			if !ok || stageLog == "" {
				return fmt.Sprintf("Stage %q has no logs.", details.Name), nil
			}
			return maybeTrim(stageLog, tail), nil
		}
		// Fetch every node's log concurrently (bounded), preserving stage order.
		nodes := details.StageFlowNodes
		logs := make([]string, len(nodes))
		errs := make([]error, len(nodes))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i := range nodes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				logs[i], _, errs[i] = c.getPipelineNodeLog(ctx, jobPath, buildNumber, nodes[i].ID)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				return "", fmt.Errorf("fetching the log of step %q (node %s) in stage %q: %w", nodes[i].Name, nodes[i].ID, details.Name, err)
			}
		}

		parts := []string{fmt.Sprintf("Stage: %s (%s)", details.Name, details.Status)}
		for i, n := range nodes {
			if strings.TrimSpace(logs[i]) != "" {
				parts = append(parts, "", fmt.Sprintf("── %s [%s] ──", n.Name, n.Status), logs[i])
				if n.Error != nil && n.Error.Message != "" {
					parts = append(parts, "↳ "+n.Error.Message)
				}
			} else if n.Error != nil && n.Error.Message != "" {
				parts = append(parts, "", fmt.Sprintf("── %s [%s] ──", n.Name, n.Status), "↳ "+n.Error.Message)
			}
		}
		out := maybeTrim(strings.Join(parts, "\n"), tail)
		if out == "" {
			out = "(no stage output)"
		}
		return out, nil
	}

	log, err := c.getConsoleLog(ctx, jobPath, buildNumber, tail)
	if err != nil {
		return "", err
	}
	if log == "" {
		return "(empty log)", nil
	}
	return log, nil
}

func (c *JenkinsClient) listJob(ctx context.Context, jobPath string, limit int) (string, error) {
	builds, err := c.listBuilds(ctx, jobPath, limit)
	if err != nil {
		return "", err
	}
	if len(builds) == 0 {
		return fmt.Sprintf("No builds found for %q.", jobPath), nil
	}
	var l lines
	plural := "s"
	if len(builds) == 1 {
		plural = ""
	}
	l.add("%s — last %d build%s:", jobPath, len(builds), plural)
	for _, b := range builds {
		dur := "..."
		if !b.Building {
			dur = fmtDuration(b.Duration)
		}
		ref := ""
		if sha := extractCommit(b.Actions); sha != "" {
			ref = " " + sha[:min(8, len(sha))]
			if branch := extractBranch(b.Actions); branch != "" {
				ref += fmt.Sprintf(" (%s)", branch)
			}
		}
		l.add("  #%-5d %-8s %7s  %s%s", b.Number, fmtResult(b.Result, b.Building), dur, fmtTimestamp(b.Timestamp), ref)
	}
	return l.String(), nil
}

func (c *JenkinsClient) listJobsTool(ctx context.Context, folder string, limit int) (string, error) {
	jobs, err := c.listJobs(ctx, folder, limit)
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		if folder != "" {
			return fmt.Sprintf("No jobs found under %q.", folder), nil
		}
		return "No top-level jobs found.", nil
	}
	var l lines
	header := "(root)"
	if folder != "" {
		header = folder
	}
	l.add("%s:", header)
	for _, j := range jobs {
		tag := fmt.Sprintf("[%s]", orDefault(j.Color, "?"))
		if knownFolderClasses[j.Class] {
			tag = "[folder]"
		}
		last := ""
		if j.LastBuild != nil {
			last = fmt.Sprintf(" last #%d", j.LastBuild.Number)
		}
		l.add("  %-20s %s%s", tag, orDefault(j.FullName, j.Name), last)
	}
	return l.String(), nil
}

func (c *JenkinsClient) getQueueTool(ctx context.Context) (string, error) {
	items, err := c.getQueue(ctx)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "Queue is empty.", nil
	}
	var l lines
	plural := "s"
	if len(items) == 1 {
		plural = ""
	}
	l.add("Queue (%d item%s):", len(items), plural)
	for _, it := range items {
		waited := "?"
		if it.InQueueSince > 0 {
			waited = fmtDuration(time.Now().UnixMilli() - it.InQueueSince)
		}
		name := orDefault(it.Task.Name, "(unnamed)")
		why := ""
		if it.Why != "" {
			why = " — " + it.Why
		}
		l.add("  %s — waiting %s%s", name, waited, why)
	}
	return l.String(), nil
}

func (c *JenkinsClient) stopBuildTool(ctx context.Context, jobPath string, buildNumber int) (string, error) {
	segs := jobPathToAPISegments(jobPath)
	if _, err := c.post(ctx, fmt.Sprintf("/%s/%d/stop", segs, buildNumber), nil, ""); err != nil {
		return "", err
	}
	return fmt.Sprintf("Stop signal sent to build #%d of %q. It may take a few seconds to terminate.", buildNumber, jobPath), nil
}

func (c *JenkinsClient) triggerBuildTool(ctx context.Context, jobPath string, parameters map[string]string) (string, error) {
	segs := jobPathToAPISegments(jobPath)
	hasParams := len(parameters) > 0
	endpoint := "build"
	var form url.Values
	if hasParams {
		endpoint = "buildWithParameters"
		form = url.Values{}
		for k, v := range parameters {
			form.Set(k, v)
		}
	}
	location, err := c.post(ctx, fmt.Sprintf("/%s/%s", segs, endpoint), form, "")
	if err != nil {
		return "", err
	}
	var l lines
	l.add("Build triggered for %q.", jobPath)
	if location != "" {
		l.add("Queue item: %s", location)
		l.add("Use jenkins_get_queue to monitor, or jenkins_list_builds to find the build number once it starts.")
	}
	return l.String(), nil
}

type gitRevision struct {
	remote string
	branch string
	sha    string
}

func (r gitRevision) String() string {
	if r.branch == "" {
		return fmt.Sprintf("%s of %s", r.sha, r.remote)
	}
	return fmt.Sprintf("%s of %s (branch %s)", r.sha, r.remote, r.branch)
}

type notifyCommitReply struct {
	scheduled []string
	polled    []string
}

func parseNotifyCommitReply(body string) notifyCommitReply {
	var r notifyCommitReply
	for line := range strings.SplitSeq(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Scheduled ")
		if !ok {
			continue
		}
		if job, polling := strings.CutPrefix(rest, "polling of "); polling {
			r.polled = append(r.polled, job)
			continue
		}
		r.scheduled = append(r.scheduled, rest)
	}
	return r
}

func (c *JenkinsClient) buildRevisionTool(ctx context.Context, rev gitRevision) (string, error) {
	q := url.Values{}
	q.Set("url", rev.remote)
	q.Set("sha1", rev.sha)
	if rev.branch != "" {
		q.Set("branches", rev.branch)
	}
	if c.notifyCommitToken != "" {
		q.Set("token", c.notifyCommitToken)
	}
	body, err := c.requestRaw(ctx, "GET", "/git/notifyCommit?"+q.Encode(), "text/plain", 0)
	if err != nil {
		return "", c.notifyCommitError(err)
	}
	text := strings.TrimSpace(string(body))
	reply := parseNotifyCommitReply(text)
	if len(reply.scheduled) == 0 && len(reply.polled) > 0 {
		return "", fmt.Errorf("Jenkins only scheduled SCM polling for %s, not a build of %s: their branch spec is parameterized, so the Git plugin ignores sha1. Trigger them with jenkins_trigger_build and the branch parameter instead", strings.Join(reply.polled, ", "), rev)
	}
	if len(reply.scheduled) == 0 {
		return "", fmt.Errorf("Jenkins scheduled no build for %s. A job reacts only when its Git remote matches the url, its branch spec matches the branch, and it has \"Poll SCM\" enabled (an empty schedule is fine) without \"Ignore post-commit hooks\". Jenkins replied: %s", rev, orDefault(text, "(empty)"))
	}
	var l lines
	l.add("Asked Jenkins to build %s.", rev)
	l.add("Scheduled %d job(s):", len(reply.scheduled))
	for _, name := range reply.scheduled {
		l.add("  - %s", name)
	}
	if len(reply.polled) > 0 {
		l.add("Only polled (parameterized branch spec, sha1 ignored): %s", strings.Join(reply.polled, ", "))
	}
	l.add("Use jenkins_get_build sha=%s to follow the build once it starts.", rev.sha)
	return l.String(), nil
}

func (c *JenkinsClient) notifyCommitError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		msg := fmt.Sprintf("Jenkins %d GET /git/notifyCommit", ae.status)
		if d := parseJenkinsErrorBody(ae.body); d != "" {
			msg += ": " + d
		}
		switch {
		case (ae.status == 401 || ae.status == 403) && c.notifyCommitToken == "":
			msg += ". Set notifyCommitToken in ~/.jenkins-mcp.json (or JENKINS_NOTIFY_COMMIT_TOKEN) to a token from Manage Jenkins -> Security -> \"Git plugin notifyCommit access tokens\"."
		case ae.status == 401 || ae.status == 403:
			msg += ". Jenkins rejected the configured notifyCommitToken; check it under Manage Jenkins -> Security -> \"Git plugin notifyCommit access tokens\"."
		case ae.status == 404:
			msg += ". Is the Git plugin installed?"
		}
		return &apiError{status: ae.status, message: msg, body: ae.body}
	}
	return err
}

func (c *JenkinsClient) getJobConfigTool(ctx context.Context, jobPath string, format outputFormat) (string, error) {
	segs := jobPathToAPISegments(jobPath)
	body, err := c.requestRaw(ctx, "GET", fmt.Sprintf("/%s/config.xml", segs), "application/xml", 0)
	if err != nil {
		return "", err
	}
	cfg, err := parseJobConfig(string(body))
	if err != nil {
		return "", err
	}
	return renderStructured(cfg, format)
}

func (c *JenkinsClient) updateJobConfigTool(ctx context.Context, jobPath string, patch jobPatch, format outputFormat) (string, error) {
	segs := jobPathToAPISegments(jobPath)
	body, err := c.requestRaw(ctx, "GET", fmt.Sprintf("/%s/config.xml", segs), "application/xml", 0)
	if err != nil {
		return "", err
	}
	merged, err := mergeJobConfig(string(body), patch)
	if err != nil {
		return "", err
	}
	if _, err := c.post(ctx, fmt.Sprintf("/%s/config.xml", segs), nil, merged); err != nil {
		return "", err
	}
	parsed, err := parseJobConfig(merged)
	if err != nil {
		return "", err
	}
	rendered, err := renderStructured(parsed, format)
	if err != nil {
		return "", fmt.Errorf("job config for %q was updated, but rendering it failed: %w", jobPath, err)
	}
	return fmt.Sprintf("Job config for %q updated.\n\nNew config:\n%s", jobPath, rendered), nil
}

func (c *JenkinsClient) getTestsTool(ctx context.Context, jobPath string, buildNumber, maxFailures int, includeStackTrace bool) (string, error) {
	report, err := c.getTestReport(ctx, jobPath, buildNumber)
	if err != nil {
		return "", err
	}
	if report == nil {
		return fmt.Sprintf("No test report for %q build #%d. The build may not publish JUnit results.", jobPath, buildNumber), nil
	}
	fail, skip, pass := report.FailCount, report.SkipCount, report.PassCount
	total := fail + skip + pass
	var l lines
	l.add("Tests for %s #%d: %d passed, %d failed, %d skipped (total %d)", jobPath, buildNumber, pass, fail, skip, total)

	type failed struct{ tc testCase }
	var failedCases []failed
	for _, suite := range report.Suites {
		for _, tc := range suite.Cases {
			if tc.Status == "FAILED" || tc.Status == "REGRESSION" {
				failedCases = append(failedCases, failed{tc})
			}
		}
	}
	if len(failedCases) == 0 && fail == 0 {
		return l.String(), nil
	}
	if len(failedCases) == 0 {
		l.blank()
		s := "s"
		if fail == 1 {
			s = ""
		}
		l.add("(%d failure%s reported but no per-case detail returned by Jenkins.)", fail, s)
		return l.String(), nil
	}
	l.blank()
	suffix := ""
	if len(failedCases) > maxFailures {
		suffix = fmt.Sprintf(", showing first %d", maxFailures)
	}
	l.add("Failed (%d%s):", len(failedCases), suffix)
	for i, f := range failedCases {
		if i >= maxFailures {
			break
		}
		tc := f.tc
		fqn := tc.Name
		if tc.ClassName != "" {
			fqn = tc.ClassName + "." + orDefault(tc.Name, "?")
		} else if fqn == "" {
			fqn = "?"
		}
		l.blank()
		l.add("✗ %s  (%s)", fqn, fmtDuration(int64(tc.Duration*1000)))
		if tc.ErrorDetails != "" {
			head := firstLines(tc.ErrorDetails, 5)
			l.add("%s", indent(truncStr(head, 800), "    "))
		}
		if includeStackTrace && tc.ErrorStackTrace != "" {
			stack := firstLines(tc.ErrorStackTrace, 15)
			l.add("%s", indent(truncStr(stack, 1200), "    "))
		}
	}
	return l.String(), nil
}

func (b *buildSummary) changeItems() []changeSetItem {
	if b.ChangeSet != nil && len(b.ChangeSet.Items) > 0 {
		return b.ChangeSet.Items
	}
	var out []changeSetItem
	for _, cs := range b.ChangeSets {
		out = append(out, cs.Items...)
	}
	return out
}

// ── helpers ───────────────────────────────────────────────────────────────────

func jobPathToAPISegments(jobPath string) string {
	var segs []string
	for _, seg := range strings.Split(jobPath, "/") {
		if seg == "" {
			continue
		}
		segs = append(segs, "job/"+url.PathEscape(seg))
	}
	return strings.Join(segs, "/")
}

func extractCommit(actions []buildAction) string {
	for _, a := range actions {
		if a.LastBuiltRevision != nil && a.LastBuiltRevision.SHA1 != "" {
			return a.LastBuiltRevision.SHA1
		}
		for _, branch := range slices.Sorted(maps.Keys(a.BuildsByBranchName)) {
			if v := a.BuildsByBranchName[branch]; v.Revision != nil && v.Revision.SHA1 != "" {
				return v.Revision.SHA1
			}
		}
	}
	return ""
}

func extractBranch(actions []buildAction) string {
	for _, a := range actions {
		if a.LastBuiltRevision != nil && len(a.LastBuiltRevision.Branch) > 0 {
			if n := a.LastBuiltRevision.Branch[0].Name; n != "" {
				return n
			}
		}
	}
	return ""
}

func extractCause(actions []buildAction) string {
	for _, a := range actions {
		if len(a.Causes) > 0 {
			var descs []string
			for _, cz := range a.Causes {
				if cz.ShortDescription != "" {
					descs = append(descs, cz.ShortDescription)
				}
			}
			if len(descs) > 0 {
				return strings.Join(descs, "; ")
			}
		}
	}
	return ""
}

type kv struct{ name, value string }

func extractParameters(actions []buildAction) ([]kv, error) {
	var out []kv
	for _, a := range actions {
		for _, p := range a.Parameters {
			if p.Name == "" {
				continue
			}
			var value string
			switch v := p.Value.(type) {
			case nil:
				value = ""
			case string:
				value = v
			default:
				b, err := json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("formatting build parameter %q: %w", p.Name, err)
				}
				value = string(b)
			}
			out = append(out, kv{p.Name, value})
		}
	}
	return out, nil
}

func shaPrefixMatches(target, candidate string) bool {
	t := strings.ToLower(target)
	c := strings.ToLower(candidate)
	n := min(len(t), len(c))
	if n < 7 {
		return false
	}
	return t[:n] == c[:n]
}

func parseJenkinsErrorBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	if m := regexp.MustCompile(`(?i)<title>([^<]+)</title>`).FindStringSubmatch(trimmed); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := regexp.MustCompile(`(?i)<h1>([^<]+)</h1>`).FindStringSubmatch(trimmed); m != nil {
		return strings.TrimSpace(m[1])
	}
	if len(trimmed) > 400 {
		return trimmed[:400] + "..."
	}
	return trimmed
}

func formatJenkinsError(status int, method, path, body string) string {
	details := parseJenkinsErrorBody(body)
	prefix := fmt.Sprintf("Jenkins %d %s %s", status, method, path)
	switch status {
	case 401:
		return prefix + ". Authentication failed. Check Jenkins username + API token (generate at <jenkins>/me/configure)."
	case 403:
		return prefix + ". Permission denied. The user lacks access to this job/build, or a CSRF crumb is required (api tokens normally bypass this)."
	case 404:
		return prefix + ". Not found. Verify the jobPath uses correct nesting (e.g. \"folder/sub/job\") and the build number exists."
	case 500, 502, 503:
		return strings.TrimSpace(prefix + ". Jenkins is unavailable or returned an error. " + details)
	}
	if details != "" {
		return prefix + ". " + details
	}
	return prefix
}

func fmtDuration(ms int64) string {
	if ms <= 0 {
		return "?"
	}
	s := (ms + 500) / 1000
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	rs := s % 60
	if m < 60 {
		if rs != 0 {
			return fmt.Sprintf("%dm%ds", m, rs)
		}
		return fmt.Sprintf("%dm", m)
	}
	h := m / 60
	rm := m % 60
	if rm != 0 {
		return fmt.Sprintf("%dh%dm", h, rm)
	}
	return fmt.Sprintf("%dh", h)
}

func fmtTimestamp(ms int64) string {
	if ms == 0 {
		return "?"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05") + "Z"
}

func fmtResult(r *string, building bool) string {
	if building {
		return "BUILDING"
	}
	if r == nil || *r == "" {
		return "UNKNOWN"
	}
	return *r
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func indent(s, prefix string) string {
	parts := strings.Split(s, "\n")
	for i := range parts {
		parts[i] = prefix + parts[i]
	}
	return strings.Join(parts, "\n")
}

func parseContentRangeTotal(h string) (int64, error) {
	rng, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes ")
	i := strings.LastIndex(rng, "/")
	if !ok || i < 0 {
		return 0, fmt.Errorf("206 response with an invalid Content-Range %q", h)
	}
	if total := strings.TrimSpace(rng[i+1:]); total == "*" {
		return unknownTotal, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rng[i+1:]), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("206 response with an invalid Content-Range %q", h)
	}
	return n, nil
}

// trimPartialRunePrefix drops leading UTF-8 continuation bytes so a byte-range
// tail that began mid-character does not start with a broken rune.
func trimPartialRunePrefix(s string) string {
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

func maybeTrim(s string, tailChars int) string {
	if tailChars <= 0 || len(s) <= tailChars {
		return s
	}
	return fmt.Sprintf("... (truncated, last %d of %d chars)\n", tailChars, len(s)) + s[len(s)-tailChars:]
}

func firstLines(s string, n int) string {
	parts := strings.Split(s, "\n")
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}

func truncStr(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func truncLine(s string, max int) string {
	s = strings.Split(s, "\n")[0]
	return truncStr(s, max)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// lines is a small string-builder for newline-joined output.
type lines struct{ s []string }

func (l *lines) add(format string, args ...any) {
	if len(args) == 0 {
		l.s = append(l.s, format)
	} else {
		l.s = append(l.s, fmt.Sprintf(format, args...))
	}
}
func (l *lines) blank()         { l.s = append(l.s, "") }
func (l *lines) String() string { return strings.Join(l.s, "\n") }

// sortStrings is used by tests/helpers needing deterministic ordering.
func sortStrings(s []string) []string { sort.Strings(s); return s }
