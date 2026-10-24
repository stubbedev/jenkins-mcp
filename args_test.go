package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func toolArgsFor(t *testing.T, tool, raw string) (toolArgs, error) {
	t.Helper()
	s, ok := toolSchemas()[tool]
	if !ok {
		t.Fatalf("no tool %q", tool)
	}
	return parseToolArgs(s, []byte(raw))
}

func TestParseToolArgsRejectsWrongValues(t *testing.T) {
	cases := []struct {
		tool, raw, want string
	}{
		{"jenkins_get_log", `{"buildNumber": 3, "full": "true"}`, `full must be boolean, got "true"`},
		{"jenkins_get_build", `{"includeStages": 1}`, "includeStages must be boolean, got 1"},
		{"jenkins_list_jobs", `{"limit": "ten"}`, `limit must be integer, got "ten"`},
		{"jenkins_list_jobs", `{"limit": 2.5}`, "limit must be integer, got 2.5"},
		{"jenkins_list_jobs", `{"limit": 0}`, "limit must be at least 1, got 0"},
		{"jenkins_list_jobs", `{"limit": 201}`, "limit must be at most 200, got 201"},
		{"jenkins_list_jobs", `{"limit": null}`, "limit must be integer, got null"},
		{"jenkins_list_jobs", `{"folder": ""}`, "folder must not be empty"},
		{"jenkins_list_builds", `{"jobPath": "a//b"}`, `jobPath must match ^[^/]+(/[^/]+)*$, got "a//b"`},
		{"jenkins_list_builds", `{"jobPath": "/a"}`, `jobPath must match`},
		{"jenkins_list_builds", `{"jobpath": "a"}`, `unknown argument "jobpath" (known: cwd, job, jobPath, limit, repoRoot)`},
		{"jenkins_list_builds", `{"job": "a", "jobPath": "b"}`, "pass either jobPath or its alias job, not both"},
		{"jenkins_list_builds", `{"cwd": "/a", "repoRoot": "/b"}`, "pass either cwd or its alias repoRoot, not both"},
		{"jenkins_get_log", `{}`, "buildNumber is required"},
		{"jenkins_get_log", `{"buildNumber": "lastBuild"}`, `buildNumber must match ^[1-9][0-9]*$, got "lastBuild"`},
		{"jenkins_get_build", `{"buildNumber": "latest"}`, `buildNumber must match`},
		{"jenkins_get_build", `{"buildNumber": 0}`, "buildNumber must be at least 1, got 0"},
		{"jenkins_get_build", `{"buildNumber": true}`, "buildNumber must be integer or string, got true"},
		{"jenkins_get_build", `{"sha": "abc"}`, `sha must match ^[0-9a-fA-F]{7,40}$, got "abc"`},
		{"jenkins_get_job_config", `{"format": "yaml"}`, `format must be one of ["toon","json"], got "yaml"`},
		{"jenkins_trigger_build", `{"parameters": {"ENV": 1}}`, "parameters.ENV must be string, got 1"},
		{"jenkins_trigger_build", `{"parameters": "ENV=prod"}`, `parameters must be object, got "ENV=prod"`},
		{"jenkins_update_job_config", `{"patch": {"type": "pipeline"}}`, `unknown field in patch "type"`},
		{"jenkins_update_job_config", `{"patch": {"triggers": [{"type": "cron"}]}}`, "patch.triggers[0].spec is required"},
		{"jenkins_update_job_config", `{"patch": {"triggers": [{"type": "hourly", "spec": ""}]}}`, `patch.triggers[0].type must be one of ["cron","scmPoll"], got "hourly"`},
		{"jenkins_update_job_config", `{"patch": {"buildRetention": {"numToKeep": "5"}}}`, `patch.buildRetention.numToKeep must be integer, got "5"`},
		{"jenkins_update_job_config", `{"patch": {"disabled": "no"}}`, `patch.disabled must be boolean, got "no"`},
		{"jenkins_get_queue", `[1]`, "arguments must be a JSON object, got [1]"},
		{"jenkins_get_queue", `{"x": 1}`, `unknown argument "x"`},
	}
	for _, tc := range cases {
		_, err := toolArgsFor(t, tc.tool, tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: want error containing %q, got %v", tc.tool, tc.raw, tc.want, err)
		}
	}
}

func TestParseToolArgsAppliesSchemaDefaultsAndAliases(t *testing.T) {
	cases := []struct {
		tool, raw string
		want      map[string]any
	}{
		{"jenkins_list_jobs", `{}`, map[string]any{"limit": 50}},
		{"jenkins_list_builds", `{"job": "f/j", "repoRoot": "/r"}`, map[string]any{"jobPath": "f/j", "cwd": "/r", "limit": 20}},
		{"jenkins_get_log", `{"buildNumber": "12"}`, map[string]any{"buildNumber": "12", "tailChars": 200000, "full": false}},
		{"jenkins_get_build", `{"buildNumber": 7, "sha": "ABCDEF1"}`, map[string]any{"buildNumber": 7, "sha": "ABCDEF1", "scanLimit": 30, "includeStages": true, "includeChangeSet": true, "includeArtifacts": true, "includeParameters": true}},
		{"jenkins_trigger_build", `{"parameters": {"ENV": "prod"}}`, map[string]any{"parameters": map[string]any{"ENV": "prod"}}},
		{"jenkins_get_job_config", `{}`, map[string]any{}},
	}
	for _, tc := range cases {
		got, err := toolArgsFor(t, tc.tool, tc.raw)
		if err != nil {
			t.Errorf("%s %s: %v", tc.tool, tc.raw, err)
			continue
		}
		if !reflect.DeepEqual(got.values, tc.want) {
			t.Errorf("%s %s: values = %#v, want %#v", tc.tool, tc.raw, got.values, tc.want)
		}
	}
}

func TestCompileSchemaRejectsWhatItCannotEnforce(t *testing.T) {
	cases := map[string]string{
		`{"type": "object", "additionalProperties": false, "properties": {"a": {"type": "string", "format": "uri"}}}`: `unsupported schema keyword "format"`,
		`{"type": "object", "properties": {}}`:            "object schema must set additionalProperties",
		`{"type": "array"}`:                               "array schema must set items",
		`{"type": "date"}`:                                `unsupported type "date"`,
		`{"description": "x"}`:                            "schema has no type",
		`{"type": "integer", "minimum": 1, "default": 0}`: "invalid default",
		`{"type": "object", "additionalProperties": false, "required": ["a"]}`: `required property "a" is not declared`,
	}
	for raw, want := range cases {
		v, err := decodeJSONValue([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := compileSchema("s", v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("compileSchema(%s): want error containing %q, got %v", raw, want, err)
		}
	}
}

func TestScanLimitsMatchTheSchema(t *testing.T) {
	for tool, arg := range map[string]string{"jenkins_get_build": "scanLimit", "jenkins_list_builds": "limit"} {
		if m := toolSchemas()[tool].properties[arg].maximum; m == nil || *m != maxBuildsPerRequest {
			t.Errorf("%s.%s maximum = %v, want %d", tool, arg, m, maxBuildsPerRequest)
		}
	}
}

func TestListJobsUsesTheDocumentedDefaultLimit(t *testing.T) {
	var tree string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tree = r.URL.Query().Get("tree")
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	}))
	t.Cleanup(srv.Close)
	saved := jenkins
	jenkins = NewJenkinsClient(Config{URL: srv.URL, Username: "u", Token: "t"})
	t.Cleanup(func() { jenkins = saved })

	res, err := connectTest(t).CallTool(context.Background(), &mcp.CallToolParams{Name: "jenkins_list_jobs", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool: %v %q", err, resultText(res))
	}
	if !strings.HasSuffix(tree, "{0,50}") {
		t.Errorf("tree = %q, want the schema default of 50 jobs", tree)
	}
}
