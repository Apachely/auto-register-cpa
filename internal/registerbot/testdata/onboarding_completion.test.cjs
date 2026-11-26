// Executes the inspection script embedded in the driver against the observed
// provider DOM contract. Optional source path + --observe support before/after
// transaction checks with identical inputs and no real credentials or network.
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const vm = require('node:vm');
const source = readFileSync(process.argv[2] || join(__dirname, '..', 'freeai.go'), 'utf8');
const marker = 'const freeAIInspectScript = `';
const start = source.indexOf(marker) + marker.length;
assert.ok(start >= marker.length);
const inspect = source.slice(start, source.indexOf('`', start));

function element(tag, attrs = {}, text = '', children = []) {
  const e = {tagName: tag.toUpperCase(), innerText: text, textContent: text, value: '', shown: true, ...attrs, children};
  e.getAttribute = name => e[name] || '';
  e.getBoundingClientRect = () => ({left: 0, top: 0, width: e.shown ? 200 : 0, height: e.shown ? 30 : 0});
  e.querySelectorAll = selector => descendants(e).filter(child => selector.split(',').some(part => matches(child, part.trim())));
  e.querySelector = selector => e.querySelectorAll(selector)[0] || null;
  for (const child of children) child.parentElement = e;
  return e;
}
function descendants(e) { return e.children.flatMap(child => [child, ...descendants(child)]); }
function matches(e, selector) {
  if (selector === '*') return true;
  if (selector.startsWith('#')) return e.id === selector.slice(1);
  const parsed = selector.match(/^([a-z]+)?(?:\[([\w-]+)(?:=["']?([^"'\]]+)["']?)?\])?$/i);
  return !!parsed && (!parsed[1] || e.tagName === parsed[1].toUpperCase()) &&
    (!parsed[2] || (parsed[3] === undefined ? parsed[2] in e : e[parsed[2]] === parsed[3]));
}
const key = 'sk-' + 'A'.repeat(43);
const other = 'sk-' + 'B'.repeat(43);
const setupLink = () => element('a', {href: '/setup'}, 'Finish setup — 2 minutes');
const finishLink = () => element('a', {href: '/dashboard'}, 'Finish later and go to my dashboard');
function keyRow(name = 'Default Key', value = key, shown = true) {
  return element('div', {}, '', [
    element('div', {}, '', [element('p', {}, name)]),
    element('div', {}, '', [element('code', {}, value.slice(0, 28) + '…'), element('button', {title: 'Copy key', onclick: `copyKey('${value}')`, shown})]),
  ]);
}
function page(path, text, children) {
  const body = element('body', {}, text, children);
  return vm.runInNewContext(inspect, {location: new URL('https://freeaiapikey.com' + path), document: {body, querySelectorAll: s => body.querySelectorAll(s)}, getComputedStyle: e => ({visibility: e.shown ? 'visible' : 'hidden'})});
}
const cases = [
  ['survey_without_step_heading', '/setup', 'Answer two questions, get $5\nNormal $0.00', [element('form', {id: 'surveyForm'}), finishLink()], 'onboarding_profile', false, false],
  ['survey_before_claim', '/setup', 'Step 1 of 5\nNormal $0.00 · Bonus $0.00\n+$5.00', [element('form', {id: 'surveyForm'}), finishLink()], 'onboarding_profile', false, false],
  ['survey_credit_success', '/setup', 'Step 1 of 5\nNormal $5.00\n$5.00 is in your balance.', [element('div', {id: 'surveyDone'}), finishLink()], 'onboarding_credit', true, false],
  ['credit_success_before_wallet_refresh', '/setup', 'Step 1 of 5\nNormal $0.00\n$5.00 is in your balance.', [element('div', {id: 'surveyDone'}), finishLink()], 'onboarding_credit', true, false],
  ['survey_saved_without_welcome_credit', '/setup', 'Step 1 of 5\nNormal $0.00\nThanks, your answers are saved. This account starts at $0.', [element('div', {id: 'surveyDone'}), finishLink()], 'onboarding_credit', false, false],
  ['resume_optional_tutorial', '/setup', 'Step 3 of 5\nNormal $5.00', [finishLink()], 'onboarding_credit', true, false],
  ['paid_dashboard_with_setup_banner', '/dashboard', 'NORMAL CREDITS\n\n$5.00\nDefault Key', [setupLink(), keyRow()], 'key', true, true],
  ['unpaid_dashboard', '/dashboard', 'NORMAL CREDITS\n$0.00\nDefault Key', [setupLink(), keyRow()], 'onboarding_entry', false, true],
  ['default_key_not_first', '/dashboard', 'NORMAL CREDITS\n$5.00\nSecondary key\nDefault Key', [keyRow('Secondary', other), keyRow()], 'key', true, true],
  ['truncated_copy_literal', '/dashboard', 'NORMAL CREDITS\n$5.00\nDefault Key', [keyRow('Default Key', key.slice(0,28)+'…')], 'loading', true, false],
  ['no_default_key', '/dashboard', 'NORMAL CREDITS\n$5.00\nSecondary key', [keyRow('Secondary', other)], 'loading', true, false],
  ['hidden_copy_control', '/dashboard', 'NORMAL CREDITS\n$5.00\nDefault Key', [keyRow('Default Key', key, false)], 'loading', true, false],
];
const observations = cases.map(([name,path,text,children,action,credit,hasKey]) => {
  const actual = page(path,text,children);
  if (!process.argv.includes('--observe')) {
    assert.equal(actual.action,action,name);
    assert.equal(actual.credit_confirmed,credit,name);
    assert.equal(actual.key,hasKey ? key : '',name);
  }
  return {case:name,action:actual.action,credit:actual.credit_confirmed,defaultKey:actual.key===key,otherKey:actual.key===other};
});
console.log(JSON.stringify(observations, null, 2));
if (!process.argv.includes('--observe')) console.log(`PASS ${cases.length} onboarding completion cases`);
