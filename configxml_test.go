package main

import (
	"encoding/json"
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
	if cfg.Definition == nil || cfg.Definition.Type != "scm" || *cfg.Definition.ScriptPath != "ci/Jenkinsfile" {
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
	patch := JobConfig{
		Description:    &newDesc,
		BuildRetention: &BuildRetention{NumToKeep: &num},
		Definition:     &PipelineDef{Type: definitionInline, Script: ptr("echo hi")},
	}
	out, err := mergeJobConfig(scmPipelineXML, mustPatch(t, patch))
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
	if cfg.Definition == nil || cfg.Definition.Type != definitionInline || *cfg.Definition.Script != "echo hi" {
		t.Errorf("round-trip definition = %+v", cfg.Definition)
	}
	if cfg.BuildRetention == nil || cfg.BuildRetention.NumToKeep == nil || *cfg.BuildRetention.NumToKeep != 15 {
		t.Errorf("round-trip retention = %+v", cfg.BuildRetention)
	}
}

func TestMergeChoiceParameters(t *testing.T) {
	patch := JobConfig{
		Parameters: []Parameter{
			{Type: paramChoice, Name: "env", Choices: []string{"dev", "prod"}},
			{Type: paramString, Name: "tag", Default: ptr("latest")},
		},
	}
	out, err := mergeJobConfig(scmPipelineXML, mustPatch(t, patch))
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

func ptr[T any](v T) *T { return &v }

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
		out, err := renderStructured(cfg, outputFormat(format))
		if err != nil {
			t.Fatal(err)
		}
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
		"https://u:p@ho st/r.git":          "https://xxxxx@ho st/r.git",
		"https://tok@host:bad/r.git":       "https://xxxxx@host:bad/r.git",
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

	patch := JobConfig{Triggers: append(cfg.Triggers, Trigger{Type: triggerCron, Spec: "H 2 * * *"})}
	out, err := mergeJobConfig(xml, mustPatch(t, patch))
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
		if _, err := newJobPatch(JobConfig{Triggers: []Trigger{tr}}); err == nil {
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

	patch := JobConfig{Triggers: []Trigger{{Type: triggerSCMPoll}}}
	out, err := mergeJobConfig(xml, mustPatch(t, patch))
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
	out, err := mergeJobConfig(scmPipelineXML, mustPatch(t, JobConfig{Triggers: cfg.Triggers}))
	if err != nil {
		t.Fatal(err)
	}
	if root, prop := triggerLocations(t, out); root != 0 || prop != 1 {
		t.Errorf("after patch: %d root trigger(s), %d in PipelineTriggersJobProperty; want 0 and 1:\n%s", root, prop, out)
	}
}

func TestFreestyleTriggersStayAtRoot(t *testing.T) {
	xml := freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), "", false)
	out, err := mergeJobConfig(xml, mustPatch(t, JobConfig{Triggers: []Trigger{{Type: triggerSCMPoll}}}))
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
		out, err := mergeJobConfig(xml, mustPatch(t, JobConfig{Triggers: []Trigger{{Type: triggerCron, Spec: "H 4 * * *"}}}))
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

func mustPatch(t *testing.T, cfg JobConfig) jobPatch {
	t.Helper()
	p, err := newJobPatch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewJobPatchRejectsWhatMergeCannotHonour(t *testing.T) {
	cases := map[string]struct {
		cfg  JobConfig
		want string
	}{
		"empty patch":            {JobConfig{}, "at least one field"},
		"unknown parameter type": {JobConfig{Parameters: []Parameter{{Type: "file", Name: "f"}}}, `patch.parameters[0]: unknown parameter type "file"`},
		"empty parameter name":   {JobConfig{Parameters: []Parameter{{Type: paramString, Name: " "}}}, "name must not be empty"},
		"duplicate parameter": {JobConfig{Parameters: []Parameter{{Type: paramString, Name: "A"}, {Type: paramText, Name: "A"}}},
			`patch.parameters[1]: parameter "A" is already defined at patch.parameters[0]`},
		"choice without choices": {JobConfig{Parameters: []Parameter{{Type: paramChoice, Name: "c"}}}, "needs at least one choice"},
		"choice with default":    {JobConfig{Parameters: []Parameter{{Type: paramChoice, Name: "c", Choices: []string{"a"}, Default: ptr("a")}}}, "takes no default"},
		"string with choices":    {JobConfig{Parameters: []Parameter{{Type: paramString, Name: "s", Choices: []string{"a"}}}}, "takes no choices"},
		"boolean default":        {JobConfig{Parameters: []Parameter{{Type: paramBoolean, Name: "b", Default: ptr("yes")}}}, `default must be "true" or "false", got "yes"`},
		"unknown definition":     {JobConfig{Definition: &PipelineDef{Type: "groovy"}}, `unknown definition type "groovy"`},
		"inline with scriptPath": {JobConfig{Definition: &PipelineDef{Type: definitionInline, ScriptPath: ptr("J")}}, "apply only to scm"},
		"scm with script":        {JobConfig{Definition: &PipelineDef{Type: definitionSCM, Script: ptr("x")}}, "apply only to inline"},
		"mixed untyped":          {JobConfig{Definition: &PipelineDef{Script: ptr("x"), ScriptPath: ptr("J")}}, "cannot be combined"},
		"bad trigger":            {JobConfig{Triggers: []Trigger{{Type: "github"}}}, `patch.triggers[0]: unknown trigger type "github"`},
	}
	for name, c := range cases {
		if _, err := newJobPatch(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

const multibranchXML = `<?xml version='1.1' encoding='UTF-8'?>
<org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject plugin="workflow-multibranch@1">
  <description>mb</description>
  <triggers/>
</org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject>`

func TestMergeRejectsFieldsTheJobTypeDoesNotHave(t *testing.T) {
	freestyle := freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false)
	cases := []struct {
		name, xml string
		cfg       JobConfig
		want      string
	}{
		{"definition on freestyle", freestyle, JobConfig{Definition: &PipelineDef{Type: definitionInline, Script: ptr("x")}}, "patch.definition applies only to pipeline jobs, and this job is freestyle (<project>)"},
		{"triggers on multibranch", multibranchXML, JobConfig{Triggers: []Trigger{{Type: triggerCron, Spec: "H * * * *"}}}, "patch.triggers: only description can be changed on a multibranch job"},
		{"several fields on multibranch", multibranchXML, JobConfig{Disabled: ptr(true), BuildRetention: &BuildRetention{NumToKeep: ptr(3)}}, "patch.disabled, patch.buildRetention: only description"},
	}
	for _, c := range cases {
		if _, err := mergeJobConfig(c.xml, mustPatch(t, c.cfg)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
	out, err := mergeJobConfig(multibranchXML, mustPatch(t, JobConfig{Description: ptr("new")}))
	if err != nil || !strings.Contains(out, "<description>new</description>") {
		t.Errorf("description on multibranch: %v\n%s", err, out)
	}
}

const inlinePipelineXML = `<?xml version='1.1' encoding='UTF-8'?>
<flow-definition>
  <definition class="org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition">
    <script>echo old</script>
    <sandbox>false</sandbox>
  </definition>
</flow-definition>`

func TestMergeDefinitionSemantics(t *testing.T) {
	out, err := mergeJobConfig(inlinePipelineXML, mustPatch(t, JobConfig{Definition: &PipelineDef{Script: ptr("echo new")}}))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseJobConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Definition; d == nil || d.Type != definitionInline || *d.Script != "echo new" || d.Sandbox == nil || *d.Sandbox {
		t.Errorf("editing an unsandboxed inline script must keep sandbox=false: %+v", d)
	}

	if _, err := mergeJobConfig(inlinePipelineXML, mustPatch(t, JobConfig{Definition: &PipelineDef{Type: definitionSCM, ScriptPath: ptr("Jenkinsfile")}})); err == nil || !strings.Contains(err.Error(), "this job has none") {
		t.Errorf("switching to scm without an SCM: want error, got %v", err)
	}

	scmJob := pipelineWithTriggerProperty("")
	if _, err := mergeJobConfig(scmJob, mustPatch(t, JobConfig{Definition: &PipelineDef{Type: definitionInline}})); err == nil || !strings.Contains(err.Error(), "needs script") {
		t.Errorf("switching to inline without script: want error, got %v", err)
	}
	out, err = mergeJobConfig(scmJob, mustPatch(t, JobConfig{Definition: &PipelineDef{Type: definitionInline, Script: ptr("echo hi")}}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = parseJobConfig(out); err != nil || cfg.Definition.Sandbox == nil || !*cfg.Definition.Sandbox || strings.Contains(out, "<scriptPath>") {
		t.Errorf("switching to inline enables the sandbox and drops scm fields: %+v %v\n%s", cfg.Definition, err, out)
	}

	out, err = mergeJobConfig(scmJob, mustPatch(t, JobConfig{Definition: &PipelineDef{ScriptPath: ptr("ci/Jenkinsfile")}}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = parseJobConfig(out); err != nil || cfg.Definition.Type != definitionSCM || *cfg.Definition.ScriptPath != "ci/Jenkinsfile" || len(cfg.SCM) != 1 {
		t.Errorf("updating scriptPath keeps the scm definition and its SCM: %+v %v", cfg, err)
	}
}

func TestMergeChoiceParameterHasNoDefaultValue(t *testing.T) {
	out, err := mergeJobConfig(scmPipelineXML, mustPatch(t, JobConfig{Parameters: []Parameter{{Type: paramChoice, Name: "env", Choices: []string{"dev", "prod"}}}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<defaultValue") {
		t.Errorf("choice parameters must not get a defaultValue element Jenkins ignores:\n%s", out)
	}
}

func TestParseJobConfigRejectsMalformedValues(t *testing.T) {
	cases := map[string]string{
		strings.Replace(freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false), "<disabled>false</disabled>", "<disabled>yes</disabled>", 1):       `config.xml /project/disabled must be true or false, got "yes"`,
		strings.Replace(freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false), "<ignorePostCommitHooks>false", "<ignorePostCommitHooks>False", 1): `/project/triggers/hudson.triggers.SCMTrigger/ignorePostCommitHooks must be true or false, got "False"`,
	}
	for xml, want := range cases {
		if _, err := parseJobConfig(xml); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
}

func freestyleWithRetention(discarder string) string {
	return strings.Replace(freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false), "<project>", "<project>\n  "+discarder, 1)
}

const modernRetention = `<properties>
    <jenkins.model.BuildDiscarderProperty>
      <strategy class="hudson.tasks.LogRotator">
        <daysToKeep>-1</daysToKeep>
        <numToKeep>10</numToKeep>
        <artifactDaysToKeep>-1</artifactDaysToKeep>
        <artifactNumToKeep>3</artifactNumToKeep>
      </strategy>
    </jenkins.model.BuildDiscarderProperty>
  </properties>`

func TestBuildRetentionLivesInBuildDiscarderProperty(t *testing.T) {
	xml := freestyleWithRetention(modernRetention)
	cfg, err := parseJobConfig(xml)
	if err != nil {
		t.Fatal(err)
	}
	want := &BuildRetention{DaysToKeep: ptr(-1), NumToKeep: ptr(10), ArtifactDaysToKeep: ptr(-1), ArtifactNumToKeep: ptr(3)}
	if !reflect.DeepEqual(cfg.BuildRetention, want) {
		t.Fatalf("retention = %+v, want %+v", cfg.BuildRetention, want)
	}

	out, err := mergeJobConfig(xml, mustPatch(t, JobConfig{BuildRetention: &BuildRetention{NumToKeep: ptr(25)}}))
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseJobConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	want.NumToKeep = ptr(25)
	if !reflect.DeepEqual(back.BuildRetention, want) {
		t.Errorf("patching numToKeep must keep the other limits: %+v, want %+v\n%s", back.BuildRetention, want, out)
	}
	if strings.Contains(out, "<buildDiscarder") {
		t.Errorf("no root <buildDiscarder> may be written:\n%s", out)
	}
}

func TestBuildRetentionMigratesLegacyAndBogusElements(t *testing.T) {
	legacy := freestyleWithRetention(`<logRotator class="hudson.tasks.LogRotator"><daysToKeep>7</daysToKeep><numToKeep>-1</numToKeep><artifactDaysToKeep>-1</artifactDaysToKeep><artifactNumToKeep>-1</artifactNumToKeep></logRotator>
  <buildDiscarder class="hudson.tasks.LogRotator"><strategy class="hudson.tasks.LogRotator"><numToKeep>99</numToKeep></strategy></buildDiscarder>`)
	cfg, err := parseJobConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BuildRetention == nil || *cfg.BuildRetention.DaysToKeep != 7 {
		t.Fatalf("legacy logRotator not read: %+v", cfg.BuildRetention)
	}
	out, err := mergeJobConfig(legacy, mustPatch(t, JobConfig{BuildRetention: &BuildRetention{NumToKeep: ptr(5)}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<logRotator") || strings.Contains(out, "<buildDiscarder") || !strings.Contains(out, tagBuildDiscarderProp) {
		t.Errorf("legacy and bogus elements must be replaced by the property:\n%s", out)
	}
	back, err := parseJobConfig(out)
	if err != nil || *back.BuildRetention.DaysToKeep != 7 || *back.BuildRetention.NumToKeep != 5 {
		t.Errorf("migrated retention = %+v, %v", back.BuildRetention, err)
	}

	fresh, err := mergeJobConfig(freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false), mustPatch(t, JobConfig{BuildRetention: &BuildRetention{DaysToKeep: ptr(30)}}))
	if err != nil {
		t.Fatal(err)
	}
	if back, err := parseJobConfig(fresh); err != nil || !reflect.DeepEqual(back.BuildRetention, &BuildRetention{DaysToKeep: ptr(30), NumToKeep: ptr(-1), ArtifactDaysToKeep: ptr(-1), ArtifactNumToKeep: ptr(-1)}) {
		t.Errorf("new retention = %+v, %v", back.BuildRetention, err)
	}
}

func TestBuildRetentionRejectsWhatItCannotWrite(t *testing.T) {
	other := freestyleWithRetention(`<properties><jenkins.model.BuildDiscarderProperty><strategy class="org.example.SmartDiscarder"><keep>3</keep></strategy></jenkins.model.BuildDiscarderProperty></properties>`)
	cfg, err := parseJobConfig(other)
	if err != nil || cfg.BuildRetention != nil || cfg.OtherBuildDiscarder != "org.example.SmartDiscarder" {
		t.Errorf("other discarder: retention %+v, other %q, %v", cfg.BuildRetention, cfg.OtherBuildDiscarder, err)
	}
	if _, err := mergeJobConfig(other, mustPatch(t, JobConfig{BuildRetention: &BuildRetention{NumToKeep: ptr(3)}})); err == nil || !strings.Contains(err.Error(), "org.example.SmartDiscarder") {
		t.Errorf("patching another discarder: want error, got %v", err)
	}
	for name, r := range map[string]BuildRetention{"zero": {NumToKeep: ptr(0)}, "below -1": {DaysToKeep: ptr(-2)}, "empty": {}} {
		if _, err := newJobPatch(JobConfig{BuildRetention: &r}); err == nil || !strings.Contains(err.Error(), "patch.buildRetention") {
			t.Errorf("%s: want a buildRetention error, got %v", name, err)
		}
	}
	bad := strings.Replace(freestyleWithRetention(modernRetention), "<numToKeep>10<", "<numToKeep>ten<", 1)
	if _, err := parseJobConfig(bad); err == nil || !strings.Contains(err.Error(), `strategy/numToKeep must be an integer, got "ten"`) {
		t.Errorf("malformed numToKeep: want error, got %v", err)
	}
}

func TestConcurrentBuildsFollowTheJobType(t *testing.T) {
	pipeline := pipelineWithTriggerProperty("")
	cfg, err := parseJobConfig(pipeline)
	if err != nil || cfg.ConcurrentBuilds == nil || !*cfg.ConcurrentBuilds {
		t.Fatalf("pipeline without the property allows concurrent builds: %+v, %v", cfg.ConcurrentBuilds, err)
	}
	out, err := mergeJobConfig(pipeline, mustPatch(t, JobConfig{ConcurrentBuilds: ptr(false)}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<"+tagDisableConcurrentProp) || strings.Contains(out, "<concurrentBuild>") {
		t.Errorf("disabling concurrency on a pipeline must add DisableConcurrentBuildsJobProperty:\n%s", out)
	}
	if back, err := parseJobConfig(out); err != nil || *back.ConcurrentBuilds {
		t.Errorf("after disabling: %+v, %v", back.ConcurrentBuilds, err)
	}
	again, err := mergeJobConfig(out, mustPatch(t, JobConfig{ConcurrentBuilds: ptr(true)}))
	if err != nil || strings.Contains(again, tagDisableConcurrentProp) {
		t.Errorf("enabling concurrency must remove the property: %v\n%s", err, again)
	}

	legacy := strings.Replace(pipeline, "<disabled>", "<concurrentBuild>false</concurrentBuild>\n  <disabled>", 1)
	if cfg, err := parseJobConfig(legacy); err != nil || *cfg.ConcurrentBuilds {
		t.Errorf("legacy pipeline concurrentBuild=false: %+v, %v", cfg.ConcurrentBuilds, err)
	}

	freestyle := freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false)
	if cfg, err := parseJobConfig(freestyle); err != nil || cfg.ConcurrentBuilds == nil || *cfg.ConcurrentBuilds {
		t.Errorf("freestyle without concurrentBuild runs one build at a time: %+v, %v", cfg.ConcurrentBuilds, err)
	}
	if out, err := mergeJobConfig(freestyle, mustPatch(t, JobConfig{ConcurrentBuilds: ptr(true)})); err != nil || !strings.Contains(out, "<concurrentBuild>true</concurrentBuild>") || strings.Contains(out, tagDisableConcurrentProp) {
		t.Errorf("freestyle concurrency stays at the root: %v\n%s", err, out)
	}
}

func TestDefinitionReportsJenkinsDefaultsNotInventedOnes(t *testing.T) {
	bare := `<flow-definition><definition class="org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition">` + fmt.Sprintf(gitSCMXML, "**", "") + `</definition></flow-definition>`
	cfg, err := parseJobConfig(bare)
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Definition; d == nil || d.ScriptPath != nil || d.Lightweight == nil || *d.Lightweight {
		t.Errorf("scm definition without scriptPath/lightweight: %+v; want no scriptPath and lightweight=false", d)
	}
	inline := `<flow-definition><definition class="org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition"><script>echo</script></definition></flow-definition>`
	if cfg, err := parseJobConfig(inline); err != nil || cfg.Definition.Sandbox == nil || *cfg.Definition.Sandbox {
		t.Errorf("inline definition without sandbox runs unsandboxed: %+v, %v", cfg.Definition, err)
	}
	other := `<flow-definition><definition class="org.example.TemplateFlowDefinition"/></flow-definition>`
	cfg, err = parseJobConfig(other)
	if err != nil || cfg.Definition == nil || cfg.Definition.Class != "org.example.TemplateFlowDefinition" {
		t.Errorf("unknown definition must be shown by class: %+v, %v", cfg.Definition, err)
	}
	if _, err := newJobPatch(JobConfig{Definition: &PipelineDef{Class: "x"}}); err == nil || !strings.Contains(err.Error(), "class is read-only") {
		t.Errorf("class in a patch: want error, got %v", err)
	}
}

const parameterizedXML = `<?xml version='1.1' encoding='UTF-8'?>
<project>
  <properties>
    <hudson.model.ParametersDefinitionProperty>
      <parameterDefinitions>
        <hudson.model.TextParameterDefinition><name>NOTES</name><description>n</description><defaultValue>t</defaultValue></hudson.model.TextParameterDefinition>
        <hudson.model.StringParameterDefinition><name>BRANCH</name><description>b</description><defaultValue>main</defaultValue><trim>true</trim></hudson.model.StringParameterDefinition>
        <hudson.model.FileParameterDefinition><name>UPLOAD</name><description>f</description></hudson.model.FileParameterDefinition>
        <hudson.model.PasswordParameterDefinition><name>SECRET</name><description>s</description><defaultValue>{AQAAABAAAAAQenc}</defaultValue></hudson.model.PasswordParameterDefinition>
        <hudson.model.BooleanParameterDefinition><name>DRY</name><description>d</description><defaultValue>false</defaultValue></hudson.model.BooleanParameterDefinition>
        <hudson.model.ChoiceParameterDefinition><name>ENV</name><description>e</description><choices class="java.util.Arrays$ArrayList"><a class="string-array"><string>dev</string><string>prod</string></a></choices></hudson.model.ChoiceParameterDefinition>
      </parameterDefinitions>
    </hudson.model.ParametersDefinitionProperty>
  </properties>
</project>`

func TestParametersKeepOrderSecretsAndUnknownKinds(t *testing.T) {
	cfg, err := parseJobConfig(parameterizedXML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range cfg.Parameters {
		names = append(names, p.Name)
		if p.Name == "SECRET" && (p.Default != nil || !p.DefaultHidden) {
			t.Errorf("password default must be hidden: %+v", p)
		}
	}
	if want := []string{"NOTES", "BRANCH", "SECRET", "DRY", "ENV"}; !reflect.DeepEqual(names, want) {
		t.Errorf("parameter order = %v, want %v", names, want)
	}
	if want := []OtherParameter{{Class: "hudson.model.FileParameterDefinition", Name: "UPLOAD"}}; !reflect.DeepEqual(cfg.OtherParameters, want) {
		t.Errorf("otherParameters = %+v, want %+v", cfg.OtherParameters, want)
	}
	for _, format := range []outputFormat{formatJSON, formatTOON} {
		if out, err := renderStructured(cfg, format); err != nil || strings.Contains(out, "AQAAABAAAAAQenc") {
			t.Errorf("%s output leaks the encrypted password default: %v\n%s", format, err, out)
		}
	}

	roundTrip := append([]Parameter{{Type: paramString, Name: "NEW", Default: ptr("x")}}, cfg.Parameters...)
	out, err := mergeJobConfig(parameterizedXML, mustPatch(t, JobConfig{Parameters: roundTrip}))
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"{AQAAABAAAAAQenc}", "<trim>true</trim>", "hudson.model.FileParameterDefinition"} {
		if !strings.Contains(out, keep) {
			t.Errorf("round-trip patch dropped %q:\n%s", keep, out)
		}
	}
	back, err := parseJobConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	names = names[:0]
	for _, p := range back.Parameters {
		names = append(names, p.Name)
	}
	if want := []string{"NEW", "NOTES", "BRANCH", "SECRET", "DRY", "ENV"}; !reflect.DeepEqual(names, want) {
		t.Errorf("patched order = %v, want %v", names, want)
	}

	if _, err := mergeJobConfig(parameterizedXML, mustPatch(t, JobConfig{Parameters: []Parameter{{Type: paramString, Name: "UPLOAD"}}})); err == nil || !strings.Contains(err.Error(), "FileParameterDefinition") {
		t.Errorf("replacing an unknown-kind parameter: want error, got %v", err)
	}
	for _, bad := range []Parameter{{Type: paramPassword, Name: "S", DefaultHidden: true, Default: ptr("new")}, {Type: paramString, Name: "S", DefaultHidden: true}} {
		if _, err := newJobPatch(JobConfig{Parameters: []Parameter{bad}}); err == nil || !strings.Contains(err.Error(), "defaultHidden only marks a password parameter") {
			t.Errorf("contradictory defaultHidden %+v: want error, got %v", bad, err)
		}
	}

	cleared, err := mergeJobConfig(freestyleXML(fmt.Sprintf(gitSCMXML, "**", ""), pollTrigger, false), mustPatch(t, JobConfig{Parameters: []Parameter{}}))
	if err != nil || strings.Contains(cleared, tagParamsProp) {
		t.Errorf("an empty parameter list must not leave an empty ParametersDefinitionProperty: %v\n%s", err, cleared)
	}
}

func TestGetOutputIsAValidPatch(t *testing.T) {
	fixtures := map[string]string{
		"legacy pipeline":   scmPipelineXML,
		"pipeline property": pipelineWithTriggerProperty(pollTrigger),
		"inline pipeline":   inlinePipelineXML,
		"freestyle":         freestyleWithRetention(modernRetention),
		"parameterized":     parameterizedXML,
	}
	for name, xml := range fixtures {
		cfg, err := parseJobConfig(xml)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, err := json.Marshal(map[string]any{"jobPath": "j", "patch": cfg.JobConfig})
		if err != nil {
			t.Fatal(err)
		}
		args, err := toolArgsFor(t, "jenkins_update_job_config", string(body))
		if err != nil {
			t.Errorf("%s: get output rejected by the patch schema: %v\n%s", name, err, body)
			continue
		}
		patch, err := parsePatch(args.object("patch"))
		if err != nil {
			t.Errorf("%s: get output rejected as a patch: %v", name, err)
			continue
		}
		out, err := mergeJobConfig(xml, patch)
		if err != nil {
			t.Errorf("%s: applying the unchanged config: %v", name, err)
			continue
		}
		back, err := parseJobConfig(out)
		if err != nil || !reflect.DeepEqual(back, cfg) {
			t.Errorf("%s: an unchanged round-trip changed the config:\n got %+v\nwant %+v (%v)", name, back, cfg, err)
		}
	}
}
