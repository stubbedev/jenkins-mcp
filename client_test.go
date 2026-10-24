package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func newTestClient(h http.Handler) (*JenkinsClient, *httptest.Server) {
	srv := httptest.NewServer(h)
	c := NewJenkinsClient(Config{URL: srv.URL, Username: "user", Token: "tok"})
	return c, srv
}

func TestGetBuildOverview(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/job/demo/lastBuild/api/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":42,"url":"http://x/42/","result":"FAILURE","building":false,"duration":65000,"timestamp":1700000000000,
			"actions":[{"lastBuiltRevision":{"SHA1":"deadbeefcafe","branch":[{"name":"origin/master"}]},"causes":[{"shortDescription":"Started by user"}]}]}`)
	})
	// pipeline describe → 404 (freestyle)
	mux.HandleFunc("/job/demo/42/wfapi/describe", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	c, srv := newTestClient(mux)
	defer srv.Close()

	out, err := c.getBuildOverview(context.Background(), buildOverviewArgs{
		jobPath: "demo", buildNumber: "lastBuild", includeStages: true, includeChangeSet: true, includeArtifacts: true, includeParameters: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Build:  #42", "Result: FAILURE", "deadbeefcafe (origin/master)", "Started by user", "jenkins_get_tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview missing %q:\n%s", want, out)
		}
	}
}

func TestGetTestsGraceful404(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/job/demo/7/testReport/api/json", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	c, srv := newTestClient(mux)
	defer srv.Close()

	out, err := c.getTestsTool(context.Background(), "demo", 7, 25, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No test report") {
		t.Errorf("expected graceful message, got: %s", out)
	}
}

func TestTriggerBuildSendsCrumb(t *testing.T) {
	var gotCrumb string
	mux := http.NewServeMux()
	mux.HandleFunc("/crumbIssuer/api/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"crumbRequestField":"Jenkins-Crumb","crumb":"abc123"}`)
	})
	mux.HandleFunc("/job/demo/build", func(w http.ResponseWriter, r *http.Request) {
		gotCrumb = r.Header.Get("Jenkins-Crumb")
		w.Header().Set("Location", "http://x/queue/item/99/")
		w.WriteHeader(201)
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	out, err := c.triggerBuildTool(context.Background(), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotCrumb != "abc123" {
		t.Errorf("crumb header not sent, got %q", gotCrumb)
	}
	if !strings.Contains(out, "queue/item/99") {
		t.Errorf("queue location missing: %s", out)
	}
}

func TestGetConsoleLogRangeTail(t *testing.T) {
	full := strings.Repeat("A", 1000) + "TAIL_END"
	mux := http.NewServeMux()
	mux.HandleFunc("/job/demo/5/consoleText", func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" { // full request
			fmt.Fprint(w, full)
			return
		}
		// honor "bytes=-N"
		n, _ := strconv.Atoi(strings.TrimPrefix(rng, "bytes=-"))
		if n > len(full) {
			n = len(full)
		}
		tail := full[len(full)-n:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", len(full)-n, len(full)-1, len(full)))
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, tail)
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	out, err := c.getConsoleLog(context.Background(), "demo", 5, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TAIL_END") {
		t.Errorf("tail content missing: %s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("of %d bytes", len(full))) {
		t.Errorf("expected total-size note, got: %s", out)
	}
	if strings.Count(out, "A") > 25 {
		t.Errorf("range tail returned too much — Range not honored: %d A's", strings.Count(out, "A"))
	}
}

func TestGetJobConfigRoundTrip(t *testing.T) {
	xml := `<?xml version='1.1' encoding='UTF-8'?>
<flow-definition>
  <description>hello</description>
  <definition class="org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition">
    <scriptPath>Jenkinsfile</scriptPath>
    <lightweight>true</lightweight>
  </definition>
</flow-definition>`
	mux := http.NewServeMux()
	mux.HandleFunc("/job/demo/config.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, xml)
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	// TOON default
	out, err := c.getJobConfigTool(context.Background(), "demo", "toon")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type: pipeline", "description: hello", "scriptPath: Jenkinsfile"} {
		if !strings.Contains(out, want) {
			t.Errorf("TOON config missing %q:\n%s", want, out)
		}
	}
	// JSON
	jsonOut, err := c.getJobConfigTool(context.Background(), "demo", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut, `"type": "pipeline"`) {
		t.Errorf("JSON config wrong:\n%s", jsonOut)
	}
}

func TestErrorStatusFormatting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/job/nope/lastBuild/api/json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, "<title>Not Found</title>")
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	_, err := c.getBuild(context.Background(), "nope", "lastBuild")
	if err == nil {
		t.Fatal("expected error")
	}
	if !isStatus(err, 404) {
		t.Errorf("expected typed 404, got: %v", err)
	}
}

func TestBuildRevisionNotifiesCommit(t *testing.T) {
	var got url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/git/notifyCommit", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		fmt.Fprint(w, "Scheduled Kontainer\nNo Git consumers using SCM API plugin for: https://git.example.com/k.git\n")
	})
	c, srv := newTestClient(mux)
	defer srv.Close()
	c.notifyCommitToken = "secret"

	sha := "d05eb5e4d05eb5e4d05eb5e4d05eb5e4d05eb5e4"
	out, err := c.buildRevisionTool(context.Background(), gitRevision{remote: "https://git.example.com/k.git", branch: "feature/x", sha: sha})
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"url": {"https://git.example.com/k.git"}, "branches": {"feature/x"}, "sha1": {sha}, "token": {"secret"}}
	if got.Encode() != want.Encode() {
		t.Errorf("query = %s, want %s", got.Encode(), want.Encode())
	}
	for _, s := range []string{"Scheduled 1 job(s)", "- Kontainer", "jenkins_get_build sha=" + sha} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
}

func TestBuildRevisionOmitsEmptyBranchAndToken(t *testing.T) {
	var got url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/git/notifyCommit", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		fmt.Fprint(w, "Scheduled a » b\n")
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	if _, err := c.buildRevisionTool(context.Background(), gitRevision{remote: "git@host:r.git", sha: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if got.Has("branches") || got.Has("token") {
		t.Errorf("empty branch/token should be omitted, got %s", got.Encode())
	}
}

func TestBuildRevisionNothingScheduled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/git/notifyCommit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "No git jobs using repository: git@host:r.git and branches: main\n")
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	_, err := c.buildRevisionTool(context.Background(), gitRevision{remote: "git@host:r.git", branch: "main", sha: strings.Repeat("a", 40)})
	if err == nil || !strings.Contains(err.Error(), "No git jobs using repository") || !strings.Contains(err.Error(), "Poll SCM") {
		t.Errorf("want no-jobs error with polling hint, got %v", err)
	}
}

func TestBuildRevisionTokenRequired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/git/notifyCommit", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "An access token is required. Please refer to Git plugin documentation for details.", http.StatusUnauthorized)
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	_, err := c.buildRevisionTool(context.Background(), gitRevision{remote: "git@host:r.git", sha: strings.Repeat("a", 40)})
	if !isStatus(err, 401) {
		t.Fatalf("want typed 401, got %v", err)
	}
	for _, s := range []string{"An access token is required", "notifyCommitToken", "Git plugin notifyCommit access tokens"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error missing %q: %v", s, err)
		}
	}

	c.notifyCommitToken = "leaky"
	_, err = c.buildRevisionTool(context.Background(), gitRevision{remote: "git@host:r.git", sha: strings.Repeat("a", 40)})
	if err == nil || strings.Contains(err.Error(), "leaky") || !strings.Contains(err.Error(), "rejected the configured notifyCommitToken") {
		t.Errorf("want rejected-token error without the token, got %v", err)
	}
}

func TestBuildRevisionNetworkErrorHidesToken(t *testing.T) {
	c := NewJenkinsClient(Config{URL: "http://127.0.0.1:1", Username: "u", Token: "t", NotifyCommitToken: "leaky"})
	_, err := c.buildRevisionTool(context.Background(), gitRevision{remote: "git@host:r.git", sha: strings.Repeat("a", 40)})
	if err == nil || strings.Contains(err.Error(), "leaky") {
		t.Errorf("want network error without the token, got %v", err)
	}
}

func TestBuildRevisionPollingOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/git/notifyCommit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Scheduled polling of param-job\n")
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	_, err := c.buildRevisionTool(context.Background(), gitRevision{remote: "git@host:r.git", sha: strings.Repeat("a", 40)})
	if err == nil || !strings.Contains(err.Error(), "only scheduled SCM polling for param-job") || !strings.Contains(err.Error(), "parameterized") {
		t.Errorf("want polling-only error naming the job, got %v", err)
	}
}

func TestBuildRevisionReportsPolledAlongsideScheduled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/git/notifyCommit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Scheduled folder » app\nScheduled polling of param-job\n")
	})
	c, srv := newTestClient(mux)
	defer srv.Close()

	out, err := c.buildRevisionTool(context.Background(), gitRevision{remote: "git@host:r.git", sha: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Scheduled 1 job(s)", "- folder » app", "Only polled", "param-job"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
}

func shaScanServer(t *testing.T, retained int, requested *int) *JenkinsClient {
	t.Helper()
	rangeRe := regexp.MustCompile(`\{0,(\d+)\}$`)
	mux := http.NewServeMux()
	mux.HandleFunc("/job/demo/api/json", func(w http.ResponseWriter, r *http.Request) {
		m := rangeRe.FindStringSubmatch(r.URL.Query().Get("tree"))
		if m == nil {
			t.Errorf("tree has no build range: %s", r.URL.Query().Get("tree"))
			return
		}
		n, _ := strconv.Atoi(m[1])
		*requested = n
		var builds []string
		for i := range min(n, retained) {
			builds = append(builds, fmt.Sprintf(`{"number":%d,"actions":[{"lastBuiltRevision":{"SHA1":"%s"}}]}`, retained-i, strings.Repeat("a", 40)))
		}
		fmt.Fprintf(w, `{"builds":[%s]}`, strings.Join(builds, ","))
	})
	c, srv := newTestClient(mux)
	t.Cleanup(srv.Close)
	return c
}

func TestGetBuildByShaReportsScanCoverage(t *testing.T) {
	cases := []struct {
		name          string
		retained      int
		scanLimit     int
		wantRequested int
		want          []string
	}{
		{"job retains fewer builds than the bound", 21, 200, 200, []string{"Scanned all 21 build(s) the job still retains (scanLimit 200)", "build retention"}},
		{"default bound reached", 100, 30, 30, []string{"in the 30 most recent build(s) scanned (scanLimit 30)", "increase scanLimit (max 200)"}},
		{"maximum bound reached", 300, 200, 200, []string{"in the 200 most recent build(s) scanned (scanLimit 200, the maximum)", "pass buildNumber"}},
	}
	for _, tc := range cases {
		var requested int
		c := shaScanServer(t, tc.retained, &requested)
		out, err := c.getBuildOverview(context.Background(), buildOverviewArgs{jobPath: "demo", sha: "bbbbbbbb", scanLimit: tc.scanLimit})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if requested != tc.wantRequested {
			t.Errorf("%s: requested %d builds, want %d", tc.name, requested, tc.wantRequested)
		}
		for _, s := range tc.want {
			if !strings.Contains(out, s) {
				t.Errorf("%s: output missing %q:\n%s", tc.name, s, out)
			}
		}
	}
}

func TestRequestJSONFailsLoudly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/garbage", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>proxy login</html>") })
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, `{"id":`)
	})
	c, srv := newTestClient(mux)
	defer srv.Close()
	var v map[string]any
	for path, want := range map[string]string{
		"/empty":   "GET /empty: Jenkins returned an empty body where JSON was expected",
		"/garbage": "GET /garbage: decoding the JSON response",
		"/short":   "GET /short: reading the response",
	} {
		if err := c.requestJSON(context.Background(), path+"?tree=x", &v, 0); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", path, want, err)
		}
	}
}

func TestTransportErrorsOmitTheQueryString(t *testing.T) {
	c := NewJenkinsClient(Config{URL: "http://127.0.0.1:1", Username: "u", Token: "t"})
	_, err := c.requestRaw(context.Background(), "GET", "/git/notifyCommit?token=s3cret&url=x", "text/plain", 0)
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "GET /git/notifyCommit:") {
		t.Errorf("want a transport error naming the path without its query, got %v", err)
	}
}

func TestCrumbFailuresAreReportedAndRetried(t *testing.T) {
	var crumbHits int
	status := http.StatusInternalServerError
	mux := http.NewServeMux()
	mux.HandleFunc("/crumbIssuer/api/json", func(w http.ResponseWriter, r *http.Request) {
		crumbHits++
		w.WriteHeader(status)
	})
	mux.HandleFunc("/job/demo/build", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	c, srv := newTestClient(mux)
	defer srv.Close()

	if _, err := c.triggerBuildTool(context.Background(), "demo", nil); err == nil || !strings.Contains(err.Error(), "fetching a CSRF crumb") {
		t.Fatalf("a failing crumb issuer must fail the POST, got %v", err)
	}
	status = http.StatusNotFound
	for range 2 {
		if _, err := c.triggerBuildTool(context.Background(), "demo", nil); err != nil {
			t.Fatalf("no crumb issuer (CSRF protection off) must not block the POST: %v", err)
		}
	}
	if crumbHits != 2 {
		t.Errorf("crumb issuer hit %d times; want one failed and one cached 404 lookup", crumbHits)
	}
}

func TestConsoleTailHandlesContentRange(t *testing.T) {
	cases := map[string]string{
		"bytes 90-99/*":   "last 10 bytes; Jenkins did not report the full size",
		"bytes 90-99/100": "last 10 of 100 bytes",
	}
	for header, want := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("/job/demo/1/consoleText", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", header)
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, "0123456789")
		})
		c, srv := newTestClient(mux)
		out, err := c.getConsoleLog(context.Background(), "demo", 1, 10)
		srv.Close()
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("Content-Range %q: got %q, %v; want %q", header, out, err, want)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/job/demo/1/consoleText", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "x")
	})
	c, srv := newTestClient(mux)
	defer srv.Close()
	if _, err := c.getConsoleLog(context.Background(), "demo", 1, 10); err == nil || !strings.Contains(err.Error(), "invalid Content-Range") {
		t.Errorf("206 without Content-Range: want error, got %v", err)
	}
}
