'use strict';

const fs = require('node:fs');
const path = require('node:path');

const home = process.env.HOME || '/home/agent';
const workspace = process.env.WORKSPACE_PATH || '/work/repo';
const codexHome = process.env.CODEX_HOME || path.join(home, '.codex');
const claudeConfig = path.join(home, '.claude.json');
const claudeSettingsDir = path.join(home, '.claude');
const claudeSettings = path.join(claudeSettingsDir, 'settings.json');
const codexConfig = path.join(codexHome, 'config.toml');

fs.mkdirSync(claudeSettingsDir, { recursive: true, mode: 0o700 });
fs.mkdirSync(codexHome, { recursive: true, mode: 0o700 });

if (!fs.existsSync(claudeConfig) || fs.statSync(claudeConfig).size === 0) {
  fs.writeFileSync(claudeConfig, `${JSON.stringify({
    hasCompletedOnboarding: true,
    numStartups: 1,
    theme: 'dark',
    projects: {
      [workspace]: {
        hasTrustDialogAccepted: true,
        hasCompletedProjectOnboarding: true,
        allowedTools: [],
      },
    },
  }, null, 2)}\n`, { mode: 0o600 });
}
if (!fs.existsSync(claudeSettings) || fs.statSync(claudeSettings).size === 0) {
  fs.writeFileSync(claudeSettings, '{"skipDangerousModePermissionPrompt":true}\n', { mode: 0o600 });
}

if (!fs.existsSync(codexConfig) || fs.statSync(codexConfig).size === 0) {
  const quotedWorkspace = JSON.stringify(workspace);
  const defaults = [
    'check_for_update_on_startup = false',
    '',
    '[tui]',
    'theme = "dark"',
    '',
    `[projects.${quotedWorkspace}]`,
    'trust_level = "trusted"',
    '',
    '[notice]',
    'hide_rate_limit_model_nudge = true',
    '',
  ].join('\n');
  fs.writeFileSync(codexConfig, defaults, { mode: 0o600 });
}
