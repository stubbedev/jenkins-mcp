import { XMLParser, XMLBuilder } from 'fast-xml-parser';

export interface PipelineDef {
  type: 'inline' | 'scm';
  script?: string;
  scriptPath?: string;
  lightweight?: boolean;
}

export interface Trigger {
  type: 'cron' | 'scmPoll';
  spec: string;
}

export type ParameterType = 'string' | 'boolean' | 'text' | 'choice' | 'password';

export interface Parameter {
  type: ParameterType;
  name: string;
  default?: string;
  description?: string;
  choices?: string[];
}

export interface BuildRetention {
  numToKeep?: number;
  daysToKeep?: number;
}

export interface JobConfig {
  type: 'pipeline' | 'freestyle' | 'multibranch' | 'unknown';
  description?: string;
  disabled?: boolean;
  concurrentBuilds?: boolean;
  definition?: PipelineDef;
  triggers?: Trigger[];
  parameters?: Parameter[];
  buildRetention?: BuildRetention;
}

const PARSER_OPTS = {
  ignoreAttributes: false,
  attributeNamePrefix: '@_',
  cdataPropName: '__cdata',
  parseTagValue: false,
  trimValues: true,
  isArray: (name: string) => ALWAYS_ARRAY.has(name),
};

const BUILDER_OPTS = {
  ignoreAttributes: false,
  attributeNamePrefix: '@_',
  cdataPropName: '__cdata',
  format: true,
  indentBy: '  ',
  suppressEmptyNode: false,
  processEntities: true,
};

const ALWAYS_ARRAY = new Set([
  'hudson.plugins.git.BranchSpec',
  'hudson.model.StringParameterDefinition',
  'hudson.model.BooleanParameterDefinition',
  'hudson.model.TextParameterDefinition',
  'hudson.model.ChoiceParameterDefinition',
  'hudson.model.PasswordParameterDefinition',
  'hudson.triggers.TimerTrigger',
  'hudson.triggers.SCMTrigger',
]);

const PARAM_CLASS: Record<ParameterType, string> = {
  string:  'hudson.model.StringParameterDefinition',
  boolean: 'hudson.model.BooleanParameterDefinition',
  text:    'hudson.model.TextParameterDefinition',
  choice:  'hudson.model.ChoiceParameterDefinition',
  password:'hudson.model.PasswordParameterDefinition',
};

const CLASS_TO_PARAM_TYPE: Record<string, ParameterType> = Object.fromEntries(
  Object.entries(PARAM_CLASS).map(([k, v]) => [v, k as ParameterType])
);

function str(v: unknown): string | undefined {
  if (v === undefined || v === null) return undefined;
  if (typeof v === 'object' && '__cdata' in (v as object)) return String((v as Record<string, unknown>).__cdata);
  return String(v);
}

function bool(v: unknown): boolean | undefined {
  if (v === undefined || v === null) return undefined;
  return String(v).toLowerCase() === 'true';
}

function num(v: unknown): number | undefined {
  const n = Number(v);
  return isNaN(n) ? undefined : n;
}

function getRoot(parsed: Record<string, unknown>): [string, Record<string, unknown>] {
  for (const key of Object.keys(parsed)) {
    if (key !== '?xml' && key !== '!doctype') return [key, parsed[key] as Record<string, unknown>];
  }
  return ['unknown', {}];
}

function jobTypeFromTag(tag: string): JobConfig['type'] {
  if (tag === 'flow-definition') return 'pipeline';
  if (tag === 'project') return 'freestyle';
  if (tag.includes('MultiBranch') || tag.includes('multibranch')) return 'multibranch';
  return 'unknown';
}

function extractTriggers(raw: Record<string, unknown> | undefined): Trigger[] {
  if (!raw) return [];
  const out: Trigger[] = [];
  const timers = (raw['hudson.triggers.TimerTrigger'] as Array<Record<string, unknown>> | undefined) ?? [];
  for (const t of timers) {
    const spec = str(t.spec);
    if (spec) out.push({ type: 'cron', spec });
  }
  const scm = (raw['hudson.triggers.SCMTrigger'] as Array<Record<string, unknown>> | undefined) ?? [];
  for (const t of scm) {
    const spec = str(t.spec);
    if (spec) out.push({ type: 'scmPoll', spec });
  }
  return out;
}

function extractParameters(props: Record<string, unknown> | undefined): Parameter[] {
  if (!props) return [];
  const paramProp = props['hudson.model.ParametersDefinitionProperty'] as Record<string, unknown> | undefined;
  if (!paramProp) return [];
  const defs = paramProp.parameterDefinitions as Record<string, unknown> | undefined;
  if (!defs) return [];

  const out: Parameter[] = [];
  for (const [xmlTag, paramType] of Object.entries(CLASS_TO_PARAM_TYPE)) {
    const items = (defs[xmlTag] as Array<Record<string, unknown>> | undefined) ?? [];
    for (const p of items) {
      const param: Parameter = { type: paramType, name: str(p.name) ?? '' };
      const def = str(p.defaultValue ?? p.default);
      if (def !== undefined) param.default = def;
      const desc = str(p.description);
      if (desc) param.description = desc;
      if (paramType === 'choice') {
        const rawChoices = str(p.choices);
        if (rawChoices) param.choices = rawChoices.split('\n').map(s => s.trim()).filter(Boolean);
      }
      out.push(param);
    }
  }
  return out;
}

function extractRetention(discarder: Record<string, unknown> | undefined): BuildRetention | undefined {
  if (!discarder) return undefined;
  const strategy = discarder.strategy as Record<string, unknown> | undefined;
  if (!strategy) return undefined;
  const numToKeep = num(strategy.numToKeep);
  const daysToKeep = num(strategy.daysToKeep);
  const result: BuildRetention = {};
  if (numToKeep !== undefined && numToKeep >= 0) result.numToKeep = numToKeep;
  if (daysToKeep !== undefined && daysToKeep >= 0) result.daysToKeep = daysToKeep;
  return Object.keys(result).length > 0 ? result : undefined;
}

export function parseJobConfig(xml: string): JobConfig {
  const parser = new XMLParser(PARSER_OPTS);
  const parsed = parser.parse(xml) as Record<string, unknown>;
  const [tag, root] = getRoot(parsed);
  const type = jobTypeFromTag(tag);
  const cfg: JobConfig = { type };

  const desc = str(root.description);
  if (desc) cfg.description = desc;

  const disabled = bool(root.disabled);
  if (disabled !== undefined) cfg.disabled = disabled;

  const concurrent = bool(root.concurrentBuild);
  if (concurrent !== undefined) cfg.concurrentBuilds = concurrent;

  // Pipeline definition
  if (type === 'pipeline') {
    const defn = root.definition as Record<string, unknown> | undefined;
    if (defn) {
      const cls = str(defn['@_class']) ?? '';
      if (cls.includes('CpsFlowDefinition')) {
        cfg.definition = { type: 'inline', script: str(defn.script) ?? '' };
      } else if (cls.includes('CpsScmFlowDefinition')) {
        cfg.definition = {
          type: 'scm',
          scriptPath: str(defn.scriptPath) ?? 'Jenkinsfile',
          lightweight: bool(defn.lightweight) ?? true,
        };
      }
    }
  }

  const triggers = extractTriggers(root.triggers as Record<string, unknown> | undefined);
  if (triggers.length > 0) cfg.triggers = triggers;

  const parameters = extractParameters(root.properties as Record<string, unknown> | undefined);
  if (parameters.length > 0) cfg.parameters = parameters;

  const retention = extractRetention(root.buildDiscarder as Record<string, unknown> | undefined);
  if (retention) cfg.buildRetention = retention;

  return cfg;
}

function buildTriggerXml(triggers: Trigger[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const crons = triggers.filter(t => t.type === 'cron');
  const polls = triggers.filter(t => t.type === 'scmPoll');
  if (crons.length > 0) out['hudson.triggers.TimerTrigger'] = crons.map(t => ({ spec: t.spec }));
  if (polls.length > 0) out['hudson.triggers.SCMTrigger'] = polls.map(t => ({ spec: t.spec, ignorePostCommitHooks: false }));
  return out;
}

function buildParameterXml(parameters: Parameter[]): Record<string, unknown> {
  const defs: Record<string, unknown[]> = {};
  for (const p of parameters) {
    const tag = PARAM_CLASS[p.type];
    if (!tag) continue;
    const entry: Record<string, unknown> = { name: p.name, defaultValue: p.default ?? '', description: p.description ?? '' };
    if (p.type === 'choice') entry.choices = (p.choices ?? []).join('\n');
    if (!defs[tag]) defs[tag] = [];
    (defs[tag] as unknown[]).push(entry);
  }
  return {
    'hudson.model.ParametersDefinitionProperty': {
      parameterDefinitions: defs,
    },
  };
}

export function mergeJobConfig(xml: string, patch: Partial<JobConfig>): string {
  const parser = new XMLParser(PARSER_OPTS);
  const parsed = parser.parse(xml) as Record<string, unknown>;
  const [tag, root] = getRoot(parsed);

  if (patch.description !== undefined) root.description = patch.description;
  if (patch.disabled !== undefined) root.disabled = patch.disabled;
  if (patch.concurrentBuilds !== undefined) root.concurrentBuild = patch.concurrentBuilds;

  if (patch.definition !== undefined && jobTypeFromTag(tag) === 'pipeline') {
    const existing = (root.definition ?? {}) as Record<string, unknown>;
    if (patch.definition.type === 'inline') {
      existing['@_class'] = 'org.jenkinsci.plugins.workflow.csd.CpsFlowDefinition';
      existing.script = patch.definition.script ?? '';
      existing.sandbox = true;
      delete existing.scm;
      delete existing.scriptPath;
      delete existing.lightweight;
    } else if (patch.definition.type === 'scm') {
      existing['@_class'] = 'org.jenkinsci.plugins.workflow.csd.CpsScmFlowDefinition';
      if (patch.definition.scriptPath !== undefined) existing.scriptPath = patch.definition.scriptPath;
      if (patch.definition.lightweight !== undefined) existing.lightweight = patch.definition.lightweight;
      delete existing.script;
      delete existing.sandbox;
    } else {
      if (patch.definition.scriptPath !== undefined) existing.scriptPath = patch.definition.scriptPath;
      if (patch.definition.script !== undefined) existing.script = patch.definition.script;
    }
    root.definition = existing;
  }

  if (patch.triggers !== undefined) {
    root.triggers = buildTriggerXml(patch.triggers);
  }

  if (patch.parameters !== undefined) {
    const props = (root.properties ?? {}) as Record<string, unknown>;
    if (patch.parameters.length === 0) {
      delete props['hudson.model.ParametersDefinitionProperty'];
    } else {
      Object.assign(props, buildParameterXml(patch.parameters));
    }
    root.properties = props;
  }

  if (patch.buildRetention !== undefined) {
    const { numToKeep = -1, daysToKeep = -1 } = patch.buildRetention;
    root.buildDiscarder = {
      strategy: {
        '@_class': 'hudson.tasks.LogRotator',
        numToKeep,
        daysToKeep,
        artifactNumToKeep: -1,
        artifactDaysToKeep: -1,
      },
    };
  }

  parsed[tag] = root;
  const builder = new XMLBuilder(BUILDER_OPTS);
  return `<?xml version="1.0" encoding="UTF-8"?>\n${builder.build({ [tag]: root }) as string}`;
}
