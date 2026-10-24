package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		jobPath: "demo", includeStages: true, includeChangeSet: true, includeArtifacts: true, includeParameters: true,
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
