package main

import (
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

type PipelineDef struct {
	Type        string `json:"type"` // "inline" | "scm"
	Script      string `json:"script,omitempty"`
	ScriptPath  string `json:"scriptPath,omitempty"`
	Lightweight *bool  `json:"lightweight,omitempty"`
}

type Trigger struct {
	Type string `json:"type"` // "cron" | "scmPoll"
	Spec string `json:"spec"`
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
	tagTimerTrigger = "hudson.triggers.TimerTrigger"
	tagSCMTrigger   = "hudson.triggers.SCMTrigger"
	tagParamsProp   = "hudson.model.ParametersDefinitionProperty"
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
func parseJobConfig(xml string) (*JobConfig, error) {
	doc, _, err := newConfigDoc(xml)
	if err != nil {
		return nil, err
	}
	root := doc.Root()
	if root == nil {
		return &JobConfig{Type: "unknown"}, nil
	}
	cfg := &JobConfig{Type: jobTypeFromTag(root.Tag)}

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

	if trig := root.SelectElement("triggers"); trig != nil {
		for _, t := range trig.SelectElements(tagTimerTrigger) {
			if spec := childText(t, "spec"); spec != "" {
				cfg.Triggers = append(cfg.Triggers, Trigger{Type: "cron", Spec: spec})
			}
		}
		for _, t := range trig.SelectElements(tagSCMTrigger) {
			if spec := childText(t, "spec"); spec != "" {
				cfg.Triggers = append(cfg.Triggers, Trigger{Type: "scmPoll", Spec: spec})
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

	return cfg, nil
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
		removeChild(root, "triggers")
		trig := root.CreateElement("triggers")
		for _, t := range patch.Triggers {
			switch t.Type {
			case "cron":
				el := trig.CreateElement(tagTimerTrigger)
				el.CreateElement("spec").SetText(t.Spec)
			case "scmPoll":
				el := trig.CreateElement(tagSCMTrigger)
				el.CreateElement("spec").SetText(t.Spec)
				el.CreateElement("ignorePostCommitHooks").SetText("false")
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
