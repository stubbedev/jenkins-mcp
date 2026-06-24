package main

import (
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
