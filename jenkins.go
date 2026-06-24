package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
}

func (e *apiError) Error() string { return e.message }

func isStatus(err error, status int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == status
}

const (
	defaultLogTailChars = 200_000
	requestTimeout      = 30 * time.Second
	logRequestTimeout   = 90 * time.Second
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
	baseURL    string
	authHeader string
	repoJobMap map[string][]string
	http       *http.Client

	mu         sync.Mutex // guards the caches below (worker pool runs handlers concurrently)
	userCache  *struct{ id, fullName string }
	crumbCache *crumb
	crumbTried bool
}

type crumb struct {
	headerName string
	value      string
}

func NewJenkinsClient(baseURL, username, token string, repoJobMap map[string][]string) *JenkinsClient {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	return &JenkinsClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: "Basic " + auth,
		repoJobMap: repoJobMap,
		http:       &http.Client{},
	}
}

// requestJSON performs a GET and decodes the JSON response into dest.
func (c *JenkinsClient) requestJSON(ctx context.Context, path string, dest any, timeout time.Duration) error {
	body, err := c.requestRaw(ctx, "GET", path, "application/json", timeout)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dest)
}

// requestRaw performs a GET and returns the raw response body. accept sets the
// Accept header. Non-2xx responses are turned into a formatted error.
func (c *JenkinsClient) requestRaw(ctx context.Context, method, path, accept string, timeout time.Duration) ([]byte, error) {
	full := path
	if !strings.HasPrefix(path, "http") {
		full = c.baseURL + path
	}
	if timeout == 0 {
		timeout = requestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, &apiError{status: res.StatusCode, message: formatJenkinsError(res.StatusCode, method, path, string(body))}
	}
	return body, nil
}

func (c *JenkinsClient) whoami(ctx context.Context) (id, fullName string) {
	c.mu.Lock()
	cached := c.userCache
	c.mu.Unlock()
	if cached != nil {
		return cached.id, cached.fullName
	}
	var me struct {
		ID       string `json:"id"`
		FullName string `json:"fullName"`
	}
	// Short timeout: whoami only feeds the startup instructions banner, so an
	// unreachable Jenkins must not stall server boot for the full 30s.
	if err := c.requestJSON(ctx, "/me/api/json", &me, 5*time.Second); err != nil {
		return "", ""
	}
	c.mu.Lock()
	c.userCache = &struct{ id, fullName string }{me.ID, me.FullName}
	c.mu.Unlock()
	return me.ID, me.FullName
}

func (c *JenkinsClient) fetchCrumb(ctx context.Context) *crumb {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.crumbTried {
		return c.crumbCache
	}
	c.crumbTried = true
	var data struct {
		CrumbRequestField string `json:"crumbRequestField"`
		Crumb             string `json:"crumb"`
	}
	if err := c.requestJSON(ctx, "/crumbIssuer/api/json", &data, 0); err == nil && data.Crumb != "" {
		c.crumbCache = &crumb{headerName: data.CrumbRequestField, value: data.Crumb}
	}
	return c.crumbCache
}

// post issues a POST with an optional form body or XML body. Returns the
// Location response header when present (e.g. queue item URL).
func (c *JenkinsClient) post(ctx context.Context, path string, form url.Values, xml string) (string, error) {
	cr := c.fetchCrumb(ctx)
	full := c.baseURL + path
	var bodyReader io.Reader
	contentType := ""
	if xml != "" {
		bodyReader = strings.NewReader(xml)
		contentType = "application/xml"
	} else if len(form) > 0 {
		bodyReader = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", full, bodyReader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cr != nil {
		req.Header.Set(cr.headerName, cr.value)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", &apiError{status: res.StatusCode, message: formatJenkinsError(res.StatusCode, "POST", path, string(body))}
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
	for needle, jobs := range c.repoJobMap {
		if strings.Contains(lc, needle) {
			for _, j := range jobs {
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

func (c *JenkinsClient) listBuilds(ctx context.Context, jobPath string, limit int) ([]buildSummary, error) {
	segs := jobPathToAPISegments(jobPath)
	rng := clamp(limit, 1, 200)
	tree := fmt.Sprintf("builds[number,url,result,building,timestamp,duration,displayName,actions[lastBuiltRevision[SHA1,branch[name]],causes[shortDescription]]]{0,%d}", rng)
	var res struct {
		Builds []buildSummary `json:"builds"`
	}
	if err := c.requestJSON(ctx, fmt.Sprintf("/%s/api/json?tree=%s", segs, url.QueryEscape(tree)), &res, 0); err != nil {
		return nil, err
	}
	return res.Builds, nil
}

func (c *JenkinsClient) findBuildBySha(ctx context.Context, jobPath, sha string, scanLimit int) (*buildSummary, error) {
	builds, err := c.listBuilds(ctx, jobPath, scanLimit)
	if err != nil {
		return nil, err
	}
	for i := range builds {
		if cand := extractCommit(builds[i].Actions); cand != "" && shaPrefixMatches(sha, cand) {
			return &builds[i], nil
		}
	}
	return nil, nil
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
	body, status, total, err := c.getRange(ctx, path, tailChars)
	if err != nil {
		return "", err
	}
	if status == http.StatusPartialContent {
		s := trimPartialRunePrefix(string(body))
		if total > int64(len(s)) {
			return fmt.Sprintf("... (truncated, last %d of %d bytes)\n", len(s), total) + s, nil
		}
		return s, nil
	}
	if status == http.StatusOK {
		s := string(body)
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

// getRange issues a GET for the last lastN bytes of path. It returns the body,
// the HTTP status (200, 206, or 416), and the total size parsed from any
// Content-Range header. Non-2xx statuses other than 416 are returned as errors.
func (c *JenkinsClient) getRange(ctx context.Context, path string, lastN int) ([]byte, int, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, logRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=-%d", lastN))
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	switch res.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return body, res.StatusCode, parseContentRangeTotal(res.Header.Get("Content-Range")), nil
	case http.StatusRequestedRangeNotSatisfiable:
		return nil, res.StatusCode, 0, nil
	default:
		return nil, res.StatusCode, 0, &apiError{status: res.StatusCode, message: formatJenkinsError(res.StatusCode, "GET", path, string(body))}
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
	rng := clamp(limit, 1, 200)
	tree := fmt.Sprintf("jobs[name,fullName,url,_class,color,buildable,lastBuild[number,url],lastCompletedBuild[number,url]]{0,%d}", rng)
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
	if a.jobPath == "" {
		return "", errors.New("jobPath is required")
	}
	var build *buildSummary
	var err error
	if a.sha != "" {
		scan := a.scanLimit
		if scan == 0 {
			scan = 30
		}
		build, err = c.findBuildBySha(ctx, a.jobPath, a.sha, scan)
		if err != nil {
			return "", err
		}
		if build == nil {
			return fmt.Sprintf("No build found in last %d builds of %q matching SHA %s. Try increasing scanLimit, or pass buildNumber explicitly.", scan, a.jobPath, a.sha), nil
		}
	} else {
		ref := a.buildNumber
		if ref == "" {
			ref = "lastBuild"
		}
		build, err = c.getBuild(ctx, a.jobPath, ref)
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
		params := extractParameters(build.Actions)
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
		if stages, _ := c.getPipelineStages(ctx, a.jobPath, build.Number); stages != nil && len(stages.Stages) > 0 {
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
	if jobPath == "" {
		return "", errors.New("jobPath is required")
	}
	if buildNumber == 0 {
		return "", errors.New("buildNumber is required")
	}
	tail := tailChars
	if full {
		tail = 0
	} else if tail == 0 {
		tail = defaultLogTailChars
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
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i := range nodes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				logs[i], _, _ = c.getPipelineNodeLog(ctx, jobPath, buildNumber, nodes[i].ID)
			}(i)
		}
		wg.Wait()

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
	if jobPath == "" {
		return "", errors.New("jobPath is required")
	}
	if limit == 0 {
		limit = 20
	}
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

func (c *JenkinsClient) getJobConfigTool(ctx context.Context, jobPath, format string) (string, error) {
	segs := jobPathToAPISegments(jobPath)
	body, err := c.requestRaw(ctx, "GET", fmt.Sprintf("/%s/config.xml", segs), "application/xml", 0)
	if err != nil {
		return "", err
	}
	cfg, err := parseJobConfig(string(body))
	if err != nil {
		return "", err
	}
	return renderStructured(cfg, format), nil
}

func (c *JenkinsClient) updateJobConfigTool(ctx context.Context, jobPath string, patch *JobConfig, format string) (string, error) {
	if patch == nil || patch.isEmpty() {
		return "", errors.New("patch is required and must contain at least one field to update.")
	}
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
	return fmt.Sprintf("Job config for %q updated.\n\nNew config:\n%s", jobPath, renderStructured(parsed, format)), nil
}

func (c *JenkinsClient) getTestsTool(ctx context.Context, jobPath string, buildNumber, maxFailures int, includeStackTrace bool) (string, error) {
	if jobPath == "" {
		return "", errors.New("jobPath is required")
	}
	if buildNumber == 0 {
		return "", errors.New("buildNumber is required")
	}
	if maxFailures == 0 {
		maxFailures = 25
	}
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

func (cfg *JobConfig) isEmpty() bool {
	return cfg.Description == nil && cfg.Disabled == nil && cfg.ConcurrentBuilds == nil &&
		cfg.Definition == nil && cfg.Triggers == nil && cfg.Parameters == nil && cfg.BuildRetention == nil
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
		for _, v := range a.BuildsByBranchName {
			if v.Revision != nil && v.Revision.SHA1 != "" {
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

func extractParameters(actions []buildAction) []kv {
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
				b, _ := json.Marshal(v)
				value = string(b)
			}
			out = append(out, kv{p.Name, value})
		}
	}
	return out
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

// parseContentRangeTotal extracts the total size from a Content-Range header
// like "bytes 1000-1999/12345", returning 0 when absent or unparseable.
func parseContentRangeTotal(h string) int64 {
	i := strings.LastIndex(h, "/")
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(h[i+1:]), 10, 64)
	if err != nil {
		return 0
	}
	return n
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

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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
