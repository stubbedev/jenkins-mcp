package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const scmPipelineXML = `<?xml version='1.0' encoding='UTF-8'?>
<flow-definition plugin="workflow-job@1234">
  <description>old desc</description>
  <keepDependencies>false</keepDependencies>
  <properties>
    <com.example.UnknownProperty><foo>keep-me</foo></com.example.UnknownProperty>
  </properties>
  <definition class="org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition" plugin="workflow-cps@1">
    <scriptPath>ci/Jenkinsfile</scriptPath>
    <lightweight>true</lightweight>
  </definition>
  <triggers>
    <hudson.triggers.TimerTrigger><spec>H 2 * * *</spec></hudson.triggers.TimerTrigger>
  </triggers>
  <disabled>false</disabled>
</flow-definition>`

func TestParsePipelineScm(t *testing.T) {
	cfg, err := parseJobConfig(scmPipelineXML)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Type != "pipeline" {
		t.Errorf("type = %q", cfg.Type)
	}
	if cfg.Definition == nil || cfg.Definition.Type != "scm" || cfg.Definition.ScriptPath != "ci/Jenkinsfile" {
		t.Errorf("definition = %+v", cfg.Definition)
	}
	if cfg.Definition.Lightweight == nil || !*cfg.Definition.Lightweight {
		t.Error("lightweight should be true")
	}
	if len(cfg.Triggers) != 1 || cfg.Triggers[0].Type != "cron" || cfg.Triggers[0].Spec != "H 2 * * *" {
		t.Errorf("triggers = %+v", cfg.Triggers)
	}
	if cfg.Description == nil || *cfg.Description != "old desc" {
		t.Errorf("description = %v", cfg.Description)
	}
}

func TestMergePreservesUnknownAndUpdates(t *testing.T) {
	newDesc := "updated"
	num := 15
	patch := &JobConfig{
		Description:    &newDesc,
		BuildRetention: &BuildRetention{NumToKeep: &num},
		Definition:     &PipelineDef{Type: "inline", Script: "echo hi"},
	}
	out, err := mergeJobConfig(scmPipelineXML, patch)
	if err != nil {
		t.Fatal(err)
	}
	// unknown element preserved
	if !strings.Contains(out, "com.example.UnknownProperty") || !strings.Contains(out, "keep-me") {
		t.Errorf("unknown property dropped:\n%s", out)
	}
	// description updated
	if !strings.Contains(out, "<description>updated</description>") {
		t.Errorf("description not updated:\n%s", out)
	}
	// switched to inline
	if !strings.Contains(out, "CpsFlowDefinition") || !strings.Contains(out, "echo hi") {
		t.Errorf("definition not switched to inline:\n%s", out)
	}
	// retention added
	if !strings.Contains(out, "<numToKeep>15</numToKeep>") {
		t.Errorf("retention missing:\n%s", out)
	}

	// re-parse the merged output: should now read back the new values
	cfg, err := parseJobConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Definition == nil || cfg.Definition.Type != "inline" || cfg.Definition.Script != "echo hi" {
		t.Errorf("round-trip definition = %+v", cfg.Definition)
	}
	if cfg.BuildRetention == nil || cfg.BuildRetention.NumToKeep == nil || *cfg.BuildRetention.NumToKeep != 15 {
		t.Errorf("round-trip retention = %+v", cfg.BuildRetention)
	}
}

func TestMergeChoiceParameters(t *testing.T) {
	patch := &JobConfig{
		Parameters: []Parameter{
			{Type: "choice", Name: "env", Choices: []string{"dev", "prod"}},
			{Type: "string", Name: "tag", Default: ptr("latest")},
		},
	}
	out, err := mergeJobConfig(scmPipelineXML, patch)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseJobConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	var choice, str *Parameter
	for i := range cfg.Parameters {
		switch cfg.Parameters[i].Name {
		case "env":
			choice = &cfg.Parameters[i]
		case "tag":
			str = &cfg.Parameters[i]
		}
	}
	if choice == nil || len(choice.Choices) != 2 || choice.Choices[0] != "dev" {
		t.Errorf("choice param round-trip failed: %+v", choice)
	}
	if str == nil || str.Default == nil || *str.Default != "latest" {
		t.Errorf("string param round-trip failed: %+v", str)
	}
}

func ptr(s string) *string { return &s }

const gitSCMXML = `<scm class="hudson.plugins.git.GitSCM" plugin="git@5.2.1">
    <configVersion>2</configVersion>
    <userRemoteConfigs>
      <hudson.plugins.git.UserRemoteConfig>
        <name>origin</name>
        <url>https://ci:s3cret@git.example.com/k.git</url>
        <credentialsId>git-creds</credentialsId>
      </hudson.plugins.git.UserRemoteConfig>
    </userRemoteConfigs>
    <branches>
      <hudson.plugins.git.BranchSpec><name>%s</name></hudson.plugins.git.BranchSpec>
    </branches>
    <extensions>%s</extensions>
  </scm>`

func freestyleXML(scm, triggers string, disabled bool) string {
	return fmt.Sprintf(`<?xml version='1.1' encoding='UTF-8'?>
<project>
  %s
  <disabled>%t</disabled>
  <triggers>%s</triggers>
</project>`, scm, disabled, triggers)
}

const pollTrigger = `<hudson.triggers.SCMTrigger><spec></spec><ignorePostCommitHooks>false</ignorePostCommitHooks></hudson.triggers.SCMTrigger>`

func TestParseFreestyleGitSCM(t *testing.T) {
	cfg, err := parseJobConfig(freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false))
	if err != nil {
		t.Fatal(err)
	}
	want := []SCMConfig{{
		Type:     "git",
		Remotes:  []GitRemote{{Name: "origin", URL: "https://ci:xxxxx@git.example.com/k.git", CredentialsID: "git-creds"}},
		Branches: []GitBranchSpec{{Name: "**"}},
	}}
	if !reflect.DeepEqual(cfg.SCM, want) {
		t.Errorf("scm = %+v, want %+v", cfg.SCM, want)
	}
	if cfg.BuildRevision.Mode != buildRevisionPinsCommit {
		t.Errorf("buildRevision = %+v, want pinsCommit", cfg.BuildRevision)
	}
	for _, format := range []string{"json", "toon"} {
		out := renderStructured(cfg, format)
		if strings.Contains(out, "s3cret") {
			t.Errorf("%s output leaks the remote password:\n%s", format, out)
		}
		for _, s := range []string{"git-creds", "pinsCommit", "k.git"} {
			if !strings.Contains(out, s) {
				t.Errorf("%s output missing %q:\n%s", format, s, out)
			}
		}
	}
}

func TestBuildRevisionSupport(t *testing.T) {
	git := func(branch, ext string) string { return fmt.Sprintf(gitSCMXML, branch, ext) }
	cases := []struct {
		name       string
		xml        string
		wantMode   BuildRevisionMode
		wantReason string
	}{
		{"wildcard spec with poll trigger", freestyleXML(git("**", ""), pollTrigger, false), buildRevisionPinsCommit, "pins the commit"},
		{"parameterized branch spec", freestyleXML(git("${BRANCH}", ""), pollTrigger, false), buildRevisionPinsCommitWithoutBranch, `"${BRANCH}" is parameterized`},
		{"no poll trigger", freestyleXML(git("**", ""), "", false), buildRevisionNotTriggered, "Poll SCM"},
		{"post-commit hooks ignored", freestyleXML(git("**", ""), `<hudson.triggers.SCMTrigger><spec/><ignorePostCommitHooks>true</ignorePostCommitHooks></hudson.triggers.SCMTrigger>`, false), buildRevisionNotTriggered, "Ignore post-commit hooks"},
		{"IgnoreNotifyCommit extension", freestyleXML(git("**", "<hudson.plugins.git.extensions.impl.IgnoreNotifyCommit/>"), pollTrigger, false), buildRevisionNotTriggered, "IgnoreNotifyCommit"},
		{"disabled job", freestyleXML(git("**", ""), pollTrigger, true), buildRevisionNotTriggered, "disabled"},
		{"no SCM", freestyleXML(`<scm class="hudson.scm.NullSCM"/>`, pollTrigger, false), buildRevisionNotTriggered, "no Git SCM"},
		{"pipeline from SCM with trigger property", `<flow-definition>
  <properties>
    <org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty>
      <triggers>` + pollTrigger + `</triggers>
    </org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty>
  </properties>
  <definition class="org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition">
    ` + git("main", "") + `
    <scriptPath>Jenkinsfile</scriptPath>
  </definition>
</flow-definition>`, buildRevisionPinsCommit, "pins the commit"},
	}
	for _, tc := range cases {
		cfg, err := parseJobConfig(tc.xml)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cfg.BuildRevision.Mode != tc.wantMode || !strings.Contains(cfg.BuildRevision.Reason, tc.wantReason) {
			t.Errorf("%s: buildRevision = %+v, want mode %s with reason containing %q", tc.name, cfg.BuildRevision, tc.wantMode, tc.wantReason)
		}
	}
}

func TestParseMultiAndForeignSCMs(t *testing.T) {
	multi := `<scm class="org.jenkinsci.plugins.multiplescms.MultiSCM">
    <scms>
      <hudson.plugins.git.GitSCM>
        <userRemoteConfigs><hudson.plugins.git.UserRemoteConfig><url>git@host:a.git</url></hudson.plugins.git.UserRemoteConfig></userRemoteConfigs>
        <branches><hudson.plugins.git.BranchSpec><name>*/main</name></hudson.plugins.git.BranchSpec></branches>
      </hudson.plugins.git.GitSCM>
      <hudson.scm.SubversionSCM><locations/></hudson.scm.SubversionSCM>
    </scms>
  </scm>`
	cfg, err := parseJobConfig(freestyleXML(multi, pollTrigger, false))
	if err != nil {
		t.Fatal(err)
	}
	want := []SCMConfig{
		{Type: "git", Remotes: []GitRemote{{URL: "git@host:a.git"}}, Branches: []GitBranchSpec{{Name: "*/main"}}},
		{Type: "hudson.scm.SubversionSCM"},
	}
	if !reflect.DeepEqual(cfg.SCM, want) {
		t.Errorf("scm = %+v, want %+v", cfg.SCM, want)
	}
	if cfg.BuildRevision.Mode != buildRevisionPinsCommit {
		t.Errorf("buildRevision = %+v, want pinsCommit from the Git member", cfg.BuildRevision)
	}
}

func TestRedactRemoteURL(t *testing.T) {
	cases := map[string]string{
		"https://u:p@host/r.git":           "https://u:xxxxx@host/r.git",
		"https://ghp_token@github.com/o/r": "https://xxxxx@github.com/o/r",
		"http://token@host/r.git":          "http://xxxxx@host/r.git",
		"ssh://git@host/r.git":             "ssh://git@host/r.git",
		"ssh://git:pw@host/r.git":          "ssh://git:xxxxx@host/r.git",
		"git@host:o/r.git":                 "git@host:o/r.git",
		"https://git.example.com/k.git":    "https://git.example.com/k.git",
		"file:///srv/git/r.git":            "file:///srv/git/r.git",
	}
	for in, want := range cases {
		if got := redactRemoteURL(in); got != want {
			t.Errorf("redactRemoteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEmptyConfigFails(t *testing.T) {
	if _, err := parseJobConfig("<?xml version='1.1'?>"); err == nil {
		t.Error("want an error for a config.xml without a root element")
	}
}

func TestTriggersRoundTripWebhookPollTrigger(t *testing.T) {
	xml := freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), `<hudson.triggers.SCMTrigger><spec></spec><ignorePostCommitHooks>false</ignorePostCommitHooks></hudson.triggers.SCMTrigger>
    <hudson.triggers.SCMTrigger><spec>H H * * *</spec><ignorePostCommitHooks>true</ignorePostCommitHooks></hudson.triggers.SCMTrigger>`, false)
	cfg, err := parseJobConfig(xml)
	if err != nil {
		t.Fatal(err)
	}
	want := []Trigger{{Type: triggerSCMPoll, Spec: ""}, {Type: triggerSCMPoll, Spec: "H H * * *", IgnorePostCommitHooks: true}}
	if !reflect.DeepEqual(cfg.Triggers, want) {
		t.Fatalf("triggers = %+v, want %+v", cfg.Triggers, want)
	}

	patch := &JobConfig{Triggers: append(cfg.Triggers, Trigger{Type: triggerCron, Spec: "H 2 * * *"})}
	out, err := mergeJobConfig(xml, patch)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseJobConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Triggers, patch.Triggers) {
		t.Errorf("round-trip triggers = %+v, want %+v", back.Triggers, patch.Triggers)
	}
	if back.BuildRevision.Mode != buildRevisionPinsCommit {
		t.Errorf("buildRevision after round-trip = %+v, want pinsCommit", back.BuildRevision)
	}
}

func TestMergeRejectsInvalidTriggers(t *testing.T) {
	for _, tr := range []Trigger{
		{Type: "github", Spec: ""},
		{Type: triggerCron, Spec: "H * * * *", IgnorePostCommitHooks: true},
	} {
		if _, err := mergeJobConfig(scmPipelineXML, &JobConfig{Triggers: []Trigger{tr}}); err == nil {
			t.Errorf("trigger %+v: want an error", tr)
		}
	}
}

func pipelineWithTriggerProperty(triggers string) string {
	return `<?xml version='1.1' encoding='UTF-8'?>
<flow-definition plugin="workflow-job@1400">
  <properties>
    <org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty>
      <triggers>` + triggers + `</triggers>
    </org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty>
  </properties>
  <definition class="org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition">
    ` + fmt.Sprintf(gitSCMXML, "**", "") + `
    <scriptPath>Jenkinsfile</scriptPath>
  </definition>
  <disabled>false</disabled>
</flow-definition>`
}

func triggerLocations(t *testing.T, xml string) (root, property int) {
	t.Helper()
	doc, _, err := newConfigDoc(xml)
	if err != nil {
		t.Fatal(err)
	}
	if trig := doc.Root().SelectElement("triggers"); trig != nil {
		root = len(trig.ChildElements())
	}
	if trig := pipelineTriggersContainer(doc.Root()); trig != nil {
		property = len(trig.ChildElements())
	}
	return root, property
}

func TestPipelineTriggersLiveInJobProperty(t *testing.T) {
	xml := pipelineWithTriggerProperty(`<hudson.triggers.TimerTrigger><spec>H 3 * * *</spec></hudson.triggers.TimerTrigger>` + pollTrigger)
	cfg, err := parseJobConfig(xml)
	if err != nil {
		t.Fatal(err)
	}
	want := []Trigger{{Type: triggerCron, Spec: "H 3 * * *"}, {Type: triggerSCMPoll}}
	if !reflect.DeepEqual(cfg.Triggers, want) {
		t.Fatalf("triggers = %+v, want %+v", cfg.Triggers, want)
	}

	patch := &JobConfig{Triggers: []Trigger{{Type: triggerSCMPoll}}}
	out, err := mergeJobConfig(xml, patch)
	if err != nil {
		t.Fatal(err)
	}
	if root, prop := triggerLocations(t, out); root != 0 || prop != 1 {
		t.Errorf("after patch: %d root trigger(s), %d in PipelineTriggersJobProperty; want 0 and 1:\n%s", root, prop, out)
	}
	back, err := parseJobConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Triggers, patch.Triggers) || back.BuildRevision.Mode != buildRevisionPinsCommit {
		t.Errorf("round-trip: triggers %+v, buildRevision %+v", back.Triggers, back.BuildRevision)
	}
}

func TestLegacyPipelineRootTriggersMoveToJobProperty(t *testing.T) {
	cfg, err := parseJobConfig(scmPipelineXML)
	if err != nil {
		t.Fatal(err)
	}
	out, err := mergeJobConfig(scmPipelineXML, &JobConfig{Triggers: cfg.Triggers})
	if err != nil {
		t.Fatal(err)
	}
	if root, prop := triggerLocations(t, out); root != 0 || prop != 1 {
		t.Errorf("after patch: %d root trigger(s), %d in PipelineTriggersJobProperty; want 0 and 1:\n%s", root, prop, out)
	}
}

func TestFreestyleTriggersStayAtRoot(t *testing.T) {
	xml := freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), "", false)
	out, err := mergeJobConfig(xml, &JobConfig{Triggers: []Trigger{{Type: triggerSCMPoll}}})
	if err != nil {
		t.Fatal(err)
	}
	if root, prop := triggerLocations(t, out); root != 1 || prop != 0 || strings.Contains(out, tagPipelineTriggersProp) {
		t.Errorf("freestyle patch: %d root trigger(s), %d in a pipeline property; want 1 and 0:\n%s", root, prop, out)
	}

	misplaced := strings.Replace(xml, "<project>", `<project>
  <properties>
    <org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty><triggers>`+pollTrigger+`</triggers></org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty>
  </properties>`, 1)
	cfg, err := parseJobConfig(misplaced)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Triggers) != 0 || cfg.BuildRevision.Mode != buildRevisionNotTriggered {
		t.Errorf("freestyle must ignore a pipeline trigger property: triggers %+v, buildRevision %+v", cfg.Triggers, cfg.BuildRevision)
	}
}

func TestTriggersPatchKeepsUnrepresentedTriggers(t *testing.T) {
	github := `<com.cloudbees.jenkins.GitHubPushTrigger plugin="github@1.40"><spec></spec></com.cloudbees.jenkins.GitHubPushTrigger>`
	cases := map[string]string{
		"freestyle":       freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), github+pollTrigger, false),
		"pipeline":        pipelineWithTriggerProperty(github + pollTrigger),
		"legacy pipeline": strings.Replace(scmPipelineXML, "<triggers>", "<triggers>"+github, 1),
	}
	for name, xml := range cases {
		cfg, err := parseJobConfig(xml)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(cfg.OtherTriggers, []string{"com.cloudbees.jenkins.GitHubPushTrigger"}) {
			t.Errorf("%s: otherTriggers = %v", name, cfg.OtherTriggers)
		}
		out, err := mergeJobConfig(xml, &JobConfig{Triggers: []Trigger{{Type: triggerCron, Spec: "H 4 * * *"}}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back, err := parseJobConfig(out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(back.OtherTriggers, cfg.OtherTriggers) || !reflect.DeepEqual(back.Triggers, []Trigger{{Type: triggerCron, Spec: "H 4 * * *"}}) {
			t.Errorf("%s: after patch triggers %+v, otherTriggers %v:\n%s", name, back.Triggers, back.OtherTriggers, out)
		}
		if _, prop := triggerLocations(t, out); name != "freestyle" && prop != 2 {
			t.Errorf("%s: want both triggers in the job property, got %d:\n%s", name, prop, out)
		}
	}
}
