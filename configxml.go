package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/beevik/etree"
)

// xmlDeclRe matches a leading XML declaration. Jenkins emits version 1.1, which
// Go's encoding/xml rejects, so the declaration is stripped before parsing and
// re-emitted verbatim on write.
var xmlDeclRe = regexp.MustCompile(`(?s)^\s*<\?xml.*?\?>`)

const defaultXMLDecl = "<?xml version='1.1' encoding='UTF-8'?>"

// splitXMLDecl separates a leading XML declaration from the document body.
func splitXMLDecl(xml string) (decl, body string) {
	if m := xmlDeclRe.FindString(xml); m != "" {
		return strings.TrimSpace(m), xml[len(m):]
	}
	return "", xml
}

// newConfigDoc parses a Jenkins config.xml into an etree document, tolerating
// XML 1.1 declarations and preserving CDATA blocks (pipeline scripts). It also
// returns the original declaration so writers can re-emit it.
func newConfigDoc(xml string) (*etree.Document, string, error) {
	decl, body := splitXMLDecl(xml)
	doc := etree.NewDocument()
	doc.ReadSettings = etree.ReadSettings{Permissive: true, PreserveCData: true}
	if err := doc.ReadFromString(body); err != nil {
		return nil, decl, err
	}
	return doc, decl, nil
}

// JobConfig is the lossy, structured view of a Jenkins job's config.xml that the
// tools expose. Reads project a subset of the XML; writes are merged back into
// the original document so unknown elements are preserved.
type JobConfig struct {
	Description      *string         `json:"description,omitempty"`
	Disabled         *bool           `json:"disabled,omitempty"`
	ConcurrentBuilds *bool           `json:"concurrentBuilds,omitempty"`
	Definition       *PipelineDef    `json:"definition,omitempty"`
	Triggers         []Trigger       `json:"triggers,omitempty"`
	Parameters       []Parameter     `json:"parameters,omitempty"`
	BuildRetention   *BuildRetention `json:"buildRetention,omitempty"`
}

type JobConfigView struct {
	Type string `json:"type"`
	JobConfig
	OtherTriggers []string             `json:"otherTriggers,omitempty"`
	SCM           []SCMConfig          `json:"scm,omitempty"`
	BuildRevision BuildRevisionSupport `json:"buildRevision"`
}

type SCMConfig struct {
	Type               string          `json:"type"`
	Remotes            []GitRemote     `json:"remotes,omitempty"`
	Branches           []GitBranchSpec `json:"branches,omitempty"`
	IgnoreNotifyCommit bool            `json:"ignoreNotifyCommit,omitempty"`
}

type GitRemote struct {
	Name          string `json:"name,omitempty"`
	URL           string `json:"url"`
	Refspec       string `json:"refspec,omitempty"`
	CredentialsID string `json:"credentialsId,omitempty"`
}

type GitBranchSpec struct {
	Name          string `json:"name"`
	Parameterized bool   `json:"parameterized,omitempty"`
}

type BuildRevisionMode string

const (
	buildRevisionPinsCommit              BuildRevisionMode = "pinsCommit"
	buildRevisionPinsCommitWithoutBranch BuildRevisionMode = "pinsCommitWithoutBranch"
	buildRevisionNotTriggered            BuildRevisionMode = "notTriggered"
)

type BuildRevisionSupport struct {
	Mode   BuildRevisionMode `json:"mode"`
	Reason string            `json:"reason"`
}

type DefinitionType string

const (
	definitionInline DefinitionType = "inline"
	definitionSCM    DefinitionType = "scm"
)

const (
	classCpsFlowDefinition    = "org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition"
	classCpsScmFlowDefinition = "org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition"
)

type PipelineDef struct {
	Type        DefinitionType `json:"type,omitempty"`
	Script      *string        `json:"script,omitempty"`
	Sandbox     *bool          `json:"sandbox,omitempty"`
	ScriptPath  *string        `json:"scriptPath,omitempty"`
	Lightweight *bool          `json:"lightweight,omitempty"`
}

func definitionTypeOf(defn *etree.Element) (DefinitionType, string) {
	if defn == nil {
		return "", ""
	}
	switch class := defn.SelectAttrValue("class", ""); class {
	case classCpsFlowDefinition:
		return definitionInline, class
	case classCpsScmFlowDefinition:
		return definitionSCM, class
	default:
		return "", class
	}
}

type TriggerType string

const (
	triggerCron    TriggerType = "cron"
	triggerSCMPoll TriggerType = "scmPoll"
)

type Trigger struct {
	Type                  TriggerType `json:"type"`
	Spec                  string      `json:"spec"`
	IgnorePostCommitHooks bool        `json:"ignorePostCommitHooks,omitempty"`
}

func (t Trigger) validate() error {
	switch t.Type {
	case triggerCron:
		if t.IgnorePostCommitHooks {
			return fmt.Errorf("trigger %q: ignorePostCommitHooks applies only to scmPoll triggers", t.Type)
		}
		return nil
	case triggerSCMPoll:
		return nil
	default:
		return fmt.Errorf("unknown trigger type %q: want %q or %q", t.Type, triggerCron, triggerSCMPoll)
	}
}

type ParameterType string

const (
	paramString   ParameterType = "string"
	paramBoolean  ParameterType = "boolean"
	paramText     ParameterType = "text"
	paramChoice   ParameterType = "choice"
	paramPassword ParameterType = "password"
)

type Parameter struct {
	Type        ParameterType `json:"type"`
	Name        string        `json:"name"`
	Default     *string       `json:"default,omitempty"`
	Description string        `json:"description,omitempty"`
	Choices     []string      `json:"choices,omitempty"`
}

type BuildRetention struct {
	NumToKeep  *int `json:"numToKeep,omitempty"`
	DaysToKeep *int `json:"daysToKeep,omitempty"`
}

const (
	tagTimerTrigger          = "hudson.triggers.TimerTrigger"
	tagSCMTrigger            = "hudson.triggers.SCMTrigger"
	tagParamsProp            = "hudson.model.ParametersDefinitionProperty"
	tagPipelineTriggersProp  = "org.jenkinsci.plugins.workflow.job.properties.PipelineTriggersJobProperty"
	classGitSCM              = "hudson.plugins.git.GitSCM"
	classMultiSCM            = "org.jenkinsci.plugins.multiplescms.MultiSCM"
	classNullSCM             = "hudson.scm.NullSCM"
	tagGitUserRemoteConfig   = "hudson.plugins.git.UserRemoteConfig"
	tagGitBranchSpec         = "hudson.plugins.git.BranchSpec"
	tagGitIgnoreNotifyCommit = "hudson.plugins.git.extensions.impl.IgnoreNotifyCommit"
	redactedUserinfo         = "xxxxx"
	scmTypeGit               = "git"
)

var paramClass = map[ParameterType]string{
	paramString:   "hudson.model.StringParameterDefinition",
	paramBoolean:  "hudson.model.BooleanParameterDefinition",
	paramText:     "hudson.model.TextParameterDefinition",
	paramChoice:   "hudson.model.ChoiceParameterDefinition",
	paramPassword: "hudson.model.PasswordParameterDefinition",
}

var classToParamType = func() map[string]ParameterType {
	m := make(map[string]ParameterType, len(paramClass))
	for k, v := range paramClass {
		m[v] = k
	}
	return m
}()

func jobTypeFromTag(tag string) string {
	switch {
	case tag == "flow-definition":
		return "pipeline"
	case tag == "project":
		return "freestyle"
	case strings.Contains(strings.ToLower(tag), "multibranch"):
		return "multibranch"
	default:
		return "unknown"
	}
}

// parseJobConfig reads a Jenkins config.xml into the structured JobConfig view.
func parseJobConfig(xml string) (*JobConfigView, error) {
	doc, _, err := newConfigDoc(xml)
	if err != nil {
		return nil, err
	}
	root := doc.Root()
	if root == nil {
		return nil, errEmptyConfig
	}
	cfg := &JobConfigView{Type: jobTypeFromTag(root.Tag)}
	var x xmlValues

	if d := childText(root, "description"); d != "" {
		cfg.Description = &d
	}
	cfg.Disabled = x.boolean(root, "disabled")
	cfg.ConcurrentBuilds = x.boolean(root, "concurrentBuild")

	if cfg.Type == "pipeline" {
		defn := root.SelectElement("definition")
		switch kind, _ := definitionTypeOf(defn); kind {
		case definitionInline:
			script := childText(defn, "script")
			def := &PipelineDef{Type: definitionInline, Script: &script}
			def.Sandbox = x.boolean(defn, "sandbox")
			cfg.Definition = def
		case definitionSCM:
			lightweight := true
			if b := x.boolean(defn, "lightweight"); b != nil {
				lightweight = *b
			}
			scriptPath := childText(defn, "scriptPath")
			if scriptPath == "" {
				scriptPath = "Jenkinsfile"
			}
			cfg.Definition = &PipelineDef{Type: definitionSCM, ScriptPath: &scriptPath, Lightweight: &lightweight}
		}
	}

	for _, trig := range triggerContainers(root) {
		for _, t := range trig.ChildElements() {
			switch t.Tag {
			case tagTimerTrigger:
				cfg.Triggers = append(cfg.Triggers, Trigger{Type: triggerCron, Spec: strings.TrimSpace(childText(t, "spec"))})
			case tagSCMTrigger:
				ignore := x.boolean(t, "ignorePostCommitHooks")
				cfg.Triggers = append(cfg.Triggers, Trigger{Type: triggerSCMPoll, Spec: strings.TrimSpace(childText(t, "spec")), IgnorePostCommitHooks: ignore != nil && *ignore})
			default:
				cfg.OtherTriggers = append(cfg.OtherTriggers, t.Tag)
			}
		}
	}

	cfg.Parameters = parseParameters(root)

	if bd := root.SelectElement("buildDiscarder"); bd != nil {
		if strat := bd.SelectElement("strategy"); strat != nil {
			ret := BuildRetention{}
			if n := x.integer(strat, "numToKeep"); n != nil && *n >= 0 {
				ret.NumToKeep = n
			}
			if n := x.integer(strat, "daysToKeep"); n != nil && *n >= 0 {
				ret.DaysToKeep = n
			}
			if ret.NumToKeep != nil || ret.DaysToKeep != nil {
				cfg.BuildRetention = &ret
			}
		}
	}

	cfg.SCM = parseSCMs(root)
	cfg.BuildRevision = buildRevisionSupport(&x, root, cfg.Disabled != nil && *cfg.Disabled, cfg.SCM)
	if x.err != nil {
		return nil, x.err
	}
	return cfg, nil
}

func scmElements(root *etree.Element) []*etree.Element {
	var out []*etree.Element
	if scm := root.SelectElement("scm"); scm != nil {
		out = append(out, scm)
	}
	if defn := root.SelectElement("definition"); defn != nil {
		if scm := defn.SelectElement("scm"); scm != nil {
			out = append(out, scm)
		}
	}
	return out
}

func scmClass(e *etree.Element) string {
	return e.SelectAttrValue("class", e.Tag)
}

func parseSCMs(root *etree.Element) []SCMConfig {
	var out []SCMConfig
	for _, e := range scmElements(root) {
		out = append(out, parseSCM(e)...)
	}
	return out
}

func parseSCM(e *etree.Element) []SCMConfig {
	switch class := scmClass(e); class {
	case classNullSCM:
		return nil
	case classGitSCM:
		return []SCMConfig{parseGitSCM(e)}
	case classMultiSCM:
		scms := e.SelectElement("scms")
		if scms == nil {
			return nil
		}
		var out []SCMConfig
		for _, child := range scms.ChildElements() {
			out = append(out, parseSCM(child)...)
		}
		return out
	default:
		return []SCMConfig{{Type: class}}
	}
}

func parseGitSCM(e *etree.Element) SCMConfig {
	scm := SCMConfig{Type: scmTypeGit}
	if remotes := e.SelectElement("userRemoteConfigs"); remotes != nil {
		for _, r := range remotes.SelectElements(tagGitUserRemoteConfig) {
			scm.Remotes = append(scm.Remotes, GitRemote{
				Name:          strings.TrimSpace(childText(r, "name")),
				URL:           redactRemoteURL(strings.TrimSpace(childText(r, "url"))),
				Refspec:       strings.TrimSpace(childText(r, "refspec")),
				CredentialsID: strings.TrimSpace(childText(r, "credentialsId")),
			})
		}
	}
	if branches := e.SelectElement("branches"); branches != nil {
		for _, b := range branches.SelectElements(tagGitBranchSpec) {
			name := strings.TrimSpace(childText(b, "name"))
			scm.Branches = append(scm.Branches, GitBranchSpec{Name: name, Parameterized: strings.Contains(name, "$")})
		}
	}
	if ext := e.SelectElement("extensions"); ext != nil {
		scm.IgnoreNotifyCommit = ext.SelectElement(tagGitIgnoreNotifyCommit) != nil
	}
	return scm
}

func redactRemoteURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return u.Redacted()
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		u.User = url.User(redactedUserinfo)
		return u.String()
	}
	return raw
}

func pipelineTriggersContainer(root *etree.Element) *etree.Element {
	props := root.SelectElement("properties")
	if props == nil {
		return nil
	}
	prop := props.SelectElement(tagPipelineTriggersProp)
	if prop == nil {
		return nil
	}
	return prop.SelectElement("triggers")
}

func triggerContainers(root *etree.Element) []*etree.Element {
	var out []*etree.Element
	if trig := root.SelectElement("triggers"); trig != nil {
		out = append(out, trig)
	}
	if jobTypeFromTag(root.Tag) != "pipeline" {
		return out
	}
	if trig := pipelineTriggersContainer(root); trig != nil {
		out = append(out, trig)
	}
	return out
}

func writableTriggerContainer(root *etree.Element) *etree.Element {
	if jobTypeFromTag(root.Tag) != "pipeline" {
		trig := root.SelectElement("triggers")
		if trig == nil {
			trig = root.CreateElement("triggers")
		}
		return trig
	}
	trig := pipelineTriggersContainer(root)
	if trig == nil {
		props := root.SelectElement("properties")
		if props == nil {
			props = root.CreateElement("properties")
		}
		prop := props.SelectElement(tagPipelineTriggersProp)
		if prop == nil {
			prop = props.CreateElement(tagPipelineTriggersProp)
		}
		trig = prop.CreateElement("triggers")
	}
	if legacy := root.SelectElement("triggers"); legacy != nil {
		for _, c := range legacy.ChildElements() {
			legacy.RemoveChild(c)
			trig.AddChild(c)
		}
		root.RemoveChild(legacy)
	}
	return trig
}

func scmTriggers(root *etree.Element) []*etree.Element {
	var out []*etree.Element
	for _, trig := range triggerContainers(root) {
		out = append(out, trig.SelectElements(tagSCMTrigger)...)
	}
	return out
}

func buildRevisionSupport(x *xmlValues, root *etree.Element, disabled bool, scms []SCMConfig) BuildRevisionSupport {
	notTriggered := func(reason string) BuildRevisionSupport {
		return BuildRevisionSupport{Mode: buildRevisionNotTriggered, Reason: reason}
	}
	if disabled {
		return notTriggered("the job is disabled, and notifyCommit never schedules disabled jobs")
	}
	var listening []SCMConfig
	hasGit := false
	for _, scm := range scms {
		if scm.Type != scmTypeGit {
			continue
		}
		hasGit = true
		if !scm.IgnoreNotifyCommit {
			listening = append(listening, scm)
		}
	}
	if !hasGit {
		return notTriggered("the job has no Git SCM, so the Git plugin's notifyCommit does not consider it")
	}
	if len(listening) == 0 {
		return notTriggered("every Git SCM has the \"Don't trigger a build on commit notifications\" (IgnoreNotifyCommit) extension")
	}
	triggers := scmTriggers(root)
	if len(triggers) == 0 {
		return notTriggered("\"Poll SCM\" is not enabled; notifyCommit only reaches jobs with an SCM polling trigger (an empty schedule is enough)")
	}
	reacting := false
	for _, t := range triggers {
		if ignore := x.boolean(t, "ignorePostCommitHooks"); ignore == nil || !*ignore {
			reacting = true
		}
	}
	if !reacting {
		return notTriggered("the \"Poll SCM\" trigger has \"Ignore post-commit hooks\" set")
	}
	for _, scm := range listening {
		for _, b := range scm.Branches {
			if b.Parameterized {
				return BuildRevisionSupport{
					Mode:   buildRevisionPinsCommitWithoutBranch,
					Reason: fmt.Sprintf("branch spec %q is parameterized: with a branch filter notifyCommit only polls and ignores sha1, so call jenkins_build_revision with an explicit sha (which sends no branch filter) to pin the commit", b.Name),
				}
			}
		}
	}
	return BuildRevisionSupport{Mode: buildRevisionPinsCommit, Reason: "jenkins_build_revision pins the commit when the notified url matches one of the remotes and the branch matches a branch spec"}
}

func parseParameters(root *etree.Element) []Parameter {
	props := root.SelectElement("properties")
	if props == nil {
		return nil
	}
	paramProp := props.SelectElement(tagParamsProp)
	if paramProp == nil {
		return nil
	}
	defs := paramProp.SelectElement("parameterDefinitions")
	if defs == nil {
		return nil
	}
	var out []Parameter
	for class, ptype := range classToParamType {
		for _, p := range defs.SelectElements(class) {
			param := Parameter{Type: ptype, Name: childText(p, "name")}
			if d := p.SelectElement("defaultValue"); d != nil {
				v := d.Text()
				param.Default = &v
			} else if d := p.SelectElement("default"); d != nil {
				v := d.Text()
				param.Default = &v
			}
			if desc := childText(p, "description"); desc != "" {
				param.Description = desc
			}
			if ptype == paramChoice {
				param.Choices = parseChoices(p)
			}
			out = append(out, param)
		}
	}
	return out
}

// parseChoices extracts choice strings from a ChoiceParameterDefinition. Jenkins
// stores them as <choices><a><string>x</string>...</a></choices>; older configs
// may store a newline-separated text body. Both are handled.
func parseChoices(p *etree.Element) []string {
	choices := p.SelectElement("choices")
	if choices == nil {
		return nil
	}
	var out []string
	if a := choices.SelectElement("a"); a != nil {
		for _, s := range a.SelectElements("string") {
			if v := strings.TrimSpace(s.Text()); v != "" {
				out = append(out, v)
			}
		}
	}
	if len(out) == 0 {
		for _, line := range strings.Split(choices.Text(), "\n") {
			if v := strings.TrimSpace(line); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// mergeJobConfig applies patch onto the original config.xml, preserving every
// element it does not touch, and returns the serialized result.
type jobPatch struct {
	cfg JobConfig
}

func newJobPatch(cfg JobConfig) (jobPatch, error) {
	if cfg.Description == nil && cfg.Disabled == nil && cfg.ConcurrentBuilds == nil &&
		cfg.Definition == nil && cfg.Triggers == nil && cfg.Parameters == nil && cfg.BuildRetention == nil {
		return jobPatch{}, errors.New("patch must contain at least one field to update")
	}
	for i, t := range cfg.Triggers {
		if err := t.validate(); err != nil {
			return jobPatch{}, fmt.Errorf("patch.triggers[%d]: %w", i, err)
		}
	}
	seen := map[string]int{}
	for i, p := range cfg.Parameters {
		if err := p.validate(); err != nil {
			return jobPatch{}, fmt.Errorf("patch.parameters[%d]: %w", i, err)
		}
		if first, dup := seen[p.Name]; dup {
			return jobPatch{}, fmt.Errorf("patch.parameters[%d]: parameter %q is already defined at patch.parameters[%d]", i, p.Name, first)
		}
		seen[p.Name] = i
	}
	if d := cfg.Definition; d != nil {
		if err := d.validate(); err != nil {
			return jobPatch{}, fmt.Errorf("patch.definition: %w", err)
		}
	}
	return jobPatch{cfg: cfg}, nil
}

func (p Parameter) validate() error {
	if _, ok := paramClass[p.Type]; !ok {
		return fmt.Errorf("unknown parameter type %q (want string, boolean, text, choice or password)", p.Type)
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("parameter name must not be empty")
	}
	switch p.Type {
	case paramChoice:
		if len(p.Choices) == 0 {
			return fmt.Errorf("choice parameter %q needs at least one choice", p.Name)
		}
		if p.Default != nil {
			return fmt.Errorf("choice parameter %q takes no default: Jenkins uses the first choice", p.Name)
		}
		return nil
	case paramBoolean:
		if p.Default != nil && *p.Default != "true" && *p.Default != "false" {
			return fmt.Errorf("boolean parameter %q: default must be \"true\" or \"false\", got %q", p.Name, *p.Default)
		}
	}
	if p.Choices != nil {
		return fmt.Errorf("%s parameter %q takes no choices", p.Type, p.Name)
	}
	return nil
}

func (d PipelineDef) validate() error {
	switch d.Type {
	case "", definitionInline, definitionSCM:
	default:
		return fmt.Errorf("unknown definition type %q (want %q or %q)", d.Type, definitionInline, definitionSCM)
	}
	if d.Type == definitionInline && (d.ScriptPath != nil || d.Lightweight != nil) {
		return errors.New("scriptPath and lightweight apply only to scm definitions")
	}
	if d.Type == definitionSCM && (d.Script != nil || d.Sandbox != nil) {
		return errors.New("script and sandbox apply only to inline definitions")
	}
	if d.Type == "" && (d.Script != nil || d.Sandbox != nil) && (d.ScriptPath != nil || d.Lightweight != nil) {
		return errors.New("script/sandbox (inline) and scriptPath/lightweight (scm) cannot be combined")
	}
	return nil
}

func patchedFields(p JobConfig) []string {
	var out []string
	add := func(set bool, name string) {
		if set {
			out = append(out, name)
		}
	}
	add(p.Disabled != nil, "disabled")
	add(p.ConcurrentBuilds != nil, "concurrentBuilds")
	add(p.Definition != nil, "definition")
	add(p.Triggers != nil, "triggers")
	add(p.Parameters != nil, "parameters")
	add(p.BuildRetention != nil, "buildRetention")
	return out
}

func checkPatchSupported(jobType, tag string, p JobConfig) error {
	if p.Definition != nil && jobType != "pipeline" {
		return fmt.Errorf("patch.definition applies only to pipeline jobs, and this job is %s (<%s>)", jobType, tag)
	}
	if jobType == "freestyle" || jobType == "pipeline" {
		return nil
	}
	if fields := patchedFields(p); len(fields) > 0 {
		return fmt.Errorf("patch.%s: only description can be changed on a %s job (<%s>); the other fields are supported for freestyle and pipeline jobs", strings.Join(fields, ", patch."), jobType, tag)
	}
	return nil
}

func mergeJobConfig(xml string, jp jobPatch) (string, error) {
	patch := jp.cfg
	doc, decl, err := newConfigDoc(xml)
	if err != nil {
		return "", err
	}
	root := doc.Root()
	if root == nil {
		return "", errEmptyConfig
	}
	if err := checkPatchSupported(jobTypeFromTag(root.Tag), root.Tag, patch); err != nil {
		return "", err
	}

	if patch.Description != nil {
		setChildText(root, "description", *patch.Description)
	}
	if patch.Disabled != nil {
		setChildText(root, "disabled", strconv.FormatBool(*patch.Disabled))
	}
	if patch.ConcurrentBuilds != nil {
		setChildText(root, "concurrentBuild", strconv.FormatBool(*patch.ConcurrentBuilds))
	}

	if patch.Definition != nil {
		if err := mergeDefinition(root, *patch.Definition); err != nil {
			return "", err
		}
	}

	if patch.Triggers != nil {
		trig := writableTriggerContainer(root)
		removeChild(trig, tagTimerTrigger)
		removeChild(trig, tagSCMTrigger)
		for _, t := range patch.Triggers {
			switch t.Type {
			case triggerCron:
				el := trig.CreateElement(tagTimerTrigger)
				el.CreateElement("spec").SetText(t.Spec)
			case triggerSCMPoll:
				el := trig.CreateElement(tagSCMTrigger)
				el.CreateElement("spec").SetText(t.Spec)
				el.CreateElement("ignorePostCommitHooks").SetText(strconv.FormatBool(t.IgnorePostCommitHooks))
			}
		}
	}

	if patch.Parameters != nil {
		props := root.SelectElement("properties")
		if props == nil {
			props = root.CreateElement("properties")
		}
		removeChild(props, tagParamsProp)
		if len(patch.Parameters) > 0 {
			prop := props.CreateElement(tagParamsProp)
			defs := prop.CreateElement("parameterDefinitions")
			for _, p := range patch.Parameters {
				el := defs.CreateElement(paramClass[p.Type])
				el.CreateElement("name").SetText(p.Name)
				if p.Type != paramChoice {
					def := ""
					if p.Default != nil {
						def = *p.Default
					}
					el.CreateElement("defaultValue").SetText(def)
				}
				el.CreateElement("description").SetText(p.Description)
				if p.Type == paramChoice {
					choices := el.CreateElement("choices")
					choices.CreateAttr("class", "java.util.Arrays$ArrayList")
					a := choices.CreateElement("a")
					a.CreateAttr("class", "string-array")
					for _, c := range p.Choices {
						a.CreateElement("string").SetText(c)
					}
				}
			}
		}
	}

	if patch.BuildRetention != nil {
		removeChild(root, "buildDiscarder")
		bd := root.CreateElement("buildDiscarder")
		bd.CreateAttr("class", "hudson.tasks.LogRotator")
		strat := bd.CreateElement("strategy")
		strat.CreateAttr("class", "hudson.tasks.LogRotator")
		num, days := -1, -1
		if patch.BuildRetention.NumToKeep != nil {
			num = *patch.BuildRetention.NumToKeep
		}
		if patch.BuildRetention.DaysToKeep != nil {
			days = *patch.BuildRetention.DaysToKeep
		}
		strat.CreateElement("daysToKeep").SetText(strconv.Itoa(days))
		strat.CreateElement("numToKeep").SetText(strconv.Itoa(num))
		strat.CreateElement("artifactDaysToKeep").SetText("-1")
		strat.CreateElement("artifactNumToKeep").SetText("-1")
	}

	doc.Indent(2)
	out, err := doc.WriteToString()
	if err != nil {
		return "", err
	}
	if decl == "" {
		decl = defaultXMLDecl
	}
	return decl + "\n" + strings.TrimLeft(out, "\n"), nil
}

func mergeDefinition(root *etree.Element, def PipelineDef) error {
	existing := root.SelectElement("definition")
	current, class := definitionTypeOf(existing)
	target := def.Type
	if target == "" {
		switch {
		case def.Script != nil || def.Sandbox != nil:
			target = definitionInline
		case def.ScriptPath != nil || def.Lightweight != nil:
			target = definitionSCM
		}
	}
	if target == "" {
		return errors.New("patch.definition needs a type or at least one field to change")
	}
	switching := current != target
	if current == "" && existing != nil {
		return fmt.Errorf("patch.definition: the job's definition is %s, which only inline and scm definitions can replace by switching type explicitly", class)
	}
	if existing == nil {
		existing = root.CreateElement("definition")
	}
	switch target {
	case definitionInline:
		if switching && def.Script == nil {
			return errors.New("patch.definition: switching to an inline definition needs script")
		}
		setAttr(existing, "class", classCpsFlowDefinition)
		if def.Script != nil {
			setChildText(existing, "script", *def.Script)
		}
		switch {
		case def.Sandbox != nil:
			setChildText(existing, "sandbox", strconv.FormatBool(*def.Sandbox))
		case switching:
			setChildText(existing, "sandbox", "true")
		}
		removeChild(existing, "scm")
		removeChild(existing, "scriptPath")
		removeChild(existing, "lightweight")
	case definitionSCM:
		if existing.SelectElement("scm") == nil {
			return errors.New("patch.definition: an scm definition needs the job's SCM, and this job has none; configure the SCM in Jenkins first")
		}
		if switching && def.ScriptPath == nil {
			return errors.New("patch.definition: switching to an scm definition needs scriptPath")
		}
		setAttr(existing, "class", classCpsScmFlowDefinition)
		if def.ScriptPath != nil {
			setChildText(existing, "scriptPath", *def.ScriptPath)
		}
		if def.Lightweight != nil {
			setChildText(existing, "lightweight", strconv.FormatBool(*def.Lightweight))
		}
		removeChild(existing, "script")
		removeChild(existing, "sandbox")
	}
	return nil
}

// ── etree helpers ───────────────────────────────────────────────────────────

func childText(e *etree.Element, tag string) string {
	if c := e.SelectElement(tag); c != nil {
		return c.Text()
	}
	return ""
}

type xmlValues struct {
	err error
}

func (x *xmlValues) fail(err error) {
	if x.err == nil {
		x.err = err
	}
}

func (x *xmlValues) boolean(e *etree.Element, tag string) *bool {
	c := e.SelectElement(tag)
	if c == nil {
		return nil
	}
	var b bool
	switch v := strings.TrimSpace(c.Text()); v {
	case "true":
		b = true
	case "false":
	default:
		x.fail(fmt.Errorf("config.xml %s must be true or false, got %q", c.GetPath(), v))
		return nil
	}
	return &b
}

func (x *xmlValues) integer(e *etree.Element, tag string) *int {
	c := e.SelectElement(tag)
	if c == nil {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(c.Text()))
	if err != nil {
		x.fail(fmt.Errorf("config.xml %s must be an integer, got %q", c.GetPath(), strings.TrimSpace(c.Text())))
		return nil
	}
	return &n
}

func setChildText(e *etree.Element, tag, value string) {
	c := e.SelectElement(tag)
	if c == nil {
		c = e.CreateElement(tag)
	}
	c.SetText(value)
}

func setAttr(e *etree.Element, key, value string) {
	if a := e.SelectAttr(key); a != nil {
		a.Value = value
		return
	}
	e.CreateAttr(key, value)
}

func removeChild(e *etree.Element, tag string) {
	for _, c := range e.SelectElements(tag) {
		e.RemoveChild(c)
	}
}
