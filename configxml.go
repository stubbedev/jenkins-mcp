package main

import (
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
	Type             string          `json:"type"`
	Description      *string         `json:"description,omitempty"`
	Disabled         *bool           `json:"disabled,omitempty"`
	ConcurrentBuilds *bool           `json:"concurrentBuilds,omitempty"`
	Definition       *PipelineDef    `json:"definition,omitempty"`
	Triggers         []Trigger       `json:"triggers,omitempty"`
	Parameters       []Parameter     `json:"parameters,omitempty"`
	BuildRetention   *BuildRetention `json:"buildRetention,omitempty"`
}

type JobConfigView struct {
	JobConfig
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

type PipelineDef struct {
	Type        string `json:"type"` // "inline" | "scm"
	Script      string `json:"script,omitempty"`
	ScriptPath  string `json:"scriptPath,omitempty"`
	Lightweight *bool  `json:"lightweight,omitempty"`
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

type Parameter struct {
	Type        string   `json:"type"` // string|boolean|text|choice|password
	Name        string   `json:"name"`
	Default     *string  `json:"default,omitempty"`
	Description string   `json:"description,omitempty"`
	Choices     []string `json:"choices,omitempty"`
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

// paramClass maps the public parameter type onto its Jenkins XML element name.
var paramClass = map[string]string{
	"string":   "hudson.model.StringParameterDefinition",
	"boolean":  "hudson.model.BooleanParameterDefinition",
	"text":     "hudson.model.TextParameterDefinition",
	"choice":   "hudson.model.ChoiceParameterDefinition",
	"password": "hudson.model.PasswordParameterDefinition",
}

// classToParamType is the reverse of paramClass.
var classToParamType = func() map[string]string {
	m := make(map[string]string, len(paramClass))
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
	cfg := &JobConfigView{JobConfig: JobConfig{Type: jobTypeFromTag(root.Tag)}}

	if d := childText(root, "description"); d != "" {
		cfg.Description = &d
	}
	if b, ok := childBool(root, "disabled"); ok {
		cfg.Disabled = &b
	}
	if b, ok := childBool(root, "concurrentBuild"); ok {
		cfg.ConcurrentBuilds = &b
	}

	if cfg.Type == "pipeline" {
		if defn := root.SelectElement("definition"); defn != nil {
			class := defn.SelectAttrValue("class", "")
			switch {
			case strings.Contains(class, "CpsFlowDefinition"):
				cfg.Definition = &PipelineDef{Type: "inline", Script: childText(defn, "script")}
			case strings.Contains(class, "CpsScmFlowDefinition"):
				lightweight := true
				if b, ok := childBool(defn, "lightweight"); ok {
					lightweight = b
				}
				scriptPath := childText(defn, "scriptPath")
				if scriptPath == "" {
					scriptPath = "Jenkinsfile"
				}
				cfg.Definition = &PipelineDef{Type: "scm", ScriptPath: scriptPath, Lightweight: &lightweight}
			}
		}
	}

	for _, trig := range triggerContainers(root) {
		for _, t := range trig.ChildElements() {
			switch t.Tag {
			case tagTimerTrigger:
				cfg.Triggers = append(cfg.Triggers, Trigger{Type: triggerCron, Spec: strings.TrimSpace(childText(t, "spec"))})
			case tagSCMTrigger:
				ignore, _ := childBool(t, "ignorePostCommitHooks")
				cfg.Triggers = append(cfg.Triggers, Trigger{Type: triggerSCMPoll, Spec: strings.TrimSpace(childText(t, "spec")), IgnorePostCommitHooks: ignore})
			}
		}
	}

	cfg.Parameters = parseParameters(root)

	if bd := root.SelectElement("buildDiscarder"); bd != nil {
		if strat := bd.SelectElement("strategy"); strat != nil {
			ret := BuildRetention{}
			if n, ok := childInt(strat, "numToKeep"); ok && n >= 0 {
				ret.NumToKeep = &n
			}
			if n, ok := childInt(strat, "daysToKeep"); ok && n >= 0 {
				ret.DaysToKeep = &n
			}
			if ret.NumToKeep != nil || ret.DaysToKeep != nil {
				cfg.BuildRetention = &ret
			}
		}
	}

	cfg.SCM = parseSCMs(root)
	cfg.BuildRevision = buildRevisionSupport(root, cfg.Disabled != nil && *cfg.Disabled, cfg.SCM)
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
	removeChild(root, "triggers")
	if trig := pipelineTriggersContainer(root); trig != nil {
		return trig
	}
	props := root.SelectElement("properties")
	if props == nil {
		props = root.CreateElement("properties")
	}
	prop := props.SelectElement(tagPipelineTriggersProp)
	if prop == nil {
		prop = props.CreateElement(tagPipelineTriggersProp)
	}
	return prop.CreateElement("triggers")
}

func scmTriggers(root *etree.Element) []*etree.Element {
	var out []*etree.Element
	for _, trig := range triggerContainers(root) {
		out = append(out, trig.SelectElements(tagSCMTrigger)...)
	}
	return out
}

func buildRevisionSupport(root *etree.Element, disabled bool, scms []SCMConfig) BuildRevisionSupport {
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
		if ignore, _ := childBool(t, "ignorePostCommitHooks"); !ignore {
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
			if ptype == "choice" {
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
func mergeJobConfig(xml string, patch *JobConfig) (string, error) {
	doc, decl, err := newConfigDoc(xml)
	if err != nil {
		return "", err
	}
	root := doc.Root()
	if root == nil {
		return "", errEmptyConfig
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

	if patch.Definition != nil && jobTypeFromTag(root.Tag) == "pipeline" {
		mergeDefinition(root, patch.Definition)
	}

	if patch.Triggers != nil {
		for _, t := range patch.Triggers {
			if err := t.validate(); err != nil {
				return "", err
			}
		}
		trig := writableTriggerContainer(root)
		for _, c := range trig.ChildElements() {
			trig.RemoveChild(c)
		}
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
				class := paramClass[p.Type]
				if class == "" {
					continue
				}
				el := defs.CreateElement(class)
				el.CreateElement("name").SetText(p.Name)
				def := ""
				if p.Default != nil {
					def = *p.Default
				}
				el.CreateElement("defaultValue").SetText(def)
				el.CreateElement("description").SetText(p.Description)
				if p.Type == "choice" {
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

func mergeDefinition(root *etree.Element, def *PipelineDef) {
	existing := root.SelectElement("definition")
	if existing == nil {
		existing = root.CreateElement("definition")
	}
	switch def.Type {
	case "inline":
		setAttr(existing, "class", "org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition")
		setChildText(existing, "script", def.Script)
		setChildText(existing, "sandbox", "true")
		removeChild(existing, "scm")
		removeChild(existing, "scriptPath")
		removeChild(existing, "lightweight")
	case "scm":
		setAttr(existing, "class", "org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition")
		if def.ScriptPath != "" {
			setChildText(existing, "scriptPath", def.ScriptPath)
		}
		if def.Lightweight != nil {
			setChildText(existing, "lightweight", strconv.FormatBool(*def.Lightweight))
		}
		removeChild(existing, "script")
		removeChild(existing, "sandbox")
	default:
		if def.ScriptPath != "" {
			setChildText(existing, "scriptPath", def.ScriptPath)
		}
		if def.Script != "" {
			setChildText(existing, "script", def.Script)
		}
	}
}

// ── etree helpers ───────────────────────────────────────────────────────────

func childText(e *etree.Element, tag string) string {
	if c := e.SelectElement(tag); c != nil {
		return c.Text()
	}
	return ""
}

func childBool(e *etree.Element, tag string) (bool, bool) {
	c := e.SelectElement(tag)
	if c == nil {
		return false, false
	}
	return strings.EqualFold(strings.TrimSpace(c.Text()), "true"), true
}

func childInt(e *etree.Element, tag string) (int, bool) {
	c := e.SelectElement(tag)
	if c == nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(c.Text()))
	if err != nil {
		return 0, false
	}
	return n, true
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
