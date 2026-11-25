// Run with: node internal/registerbot/testdata/google_identifier.test.cjs
// These DOM fixtures exercise the JavaScript actually embedded in the Go driver.
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');

function embedded(file, name) {
  const source = readFileSync(join(__dirname, '..', file), 'utf8');
  const start = source.indexOf('const ' + name + ' = `');
  assert.notEqual(start, -1);
  const content = start + ('const ' + name + ' = `').length;
  return source.slice(content, source.indexOf('`', content));
}

const inspect = embedded('freeai.go', 'freeAIInspectScript');
const helpers = embedded('cdp.go', 'helperScript');

function field(id, name, type, shown = true) {
  const attributes = {};
  return {
    tagName: 'INPUT', id, name, type, shown, innerText: '', textContent: '', value: '',
    getBoundingClientRect: () => ({ left: 10, top: 10, width: shown ? 200 : 0, height: shown ? 30 : 0 }),
    getAttribute: key => attributes[key] || '',
    setAttribute: (key, value) => { attributes[key] = value; },
    removeAttribute: key => { delete attributes[key]; },
    querySelectorAll: () => [],
  };
}

function makeContext(elements, text, url) {
  const location = new URL(url);
  const match = (element, selector) => {
    if (selector === '*') return true;
    if (selector.startsWith('#')) return element.id === selector.slice(1);
    if (selector === '[data-cpa-target]') return element.getAttribute('data-cpa-target') !== '';
    const input = selector.match(/^input(?:\[(type|name)=["']?([^"'\]]+)["']?\])?$/);
    if (input) return element.tagName === 'INPUT' && (!input[1] || element[input[1]] === input[2]);
    return false;
  };
  const document = {
    body: { innerText: text },
    scripts: [],
    querySelectorAll: selector => elements.filter(element => selector.split(',').some(part => match(element, part.trim()))),
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
  };
  const context = {
    document, location,
    getComputedStyle: element => ({ visibility: element.shown ? 'visible' : 'hidden', display: element.shown ? 'block' : 'none', opacity: '1' }),
  };
  context.window = context;
  return vm.createContext(context);
}

// Google's live identifier is a visible text input, with a hidden password
// field earlier in the document. The page also contains "sign in to" text.
const identifier = makeContext([
  field('hiddenPassword', 'Passwd', 'password', false),
  field('identifierId', 'identifier', 'text'),
], 'Sign in to FreeAIapikey\nEmail or phone\nNext', 'https://accounts.google.com/v3/signin/identifier');
assert.equal(vm.runInContext(inspect, identifier).action, 'email');
vm.runInContext(helpers, identifier);
assert.equal(vm.runInContext("cpaFindInput('#identifierId,input[name=identifier],input[type=email]', 'email')", identifier).id, 'identifierId');

// Input selection must skip the hidden first match without depending on a label.
const password = makeContext([
  field('hiddenPassword', 'Passwd', 'password', false),
  field('passwordField', 'Passwd', 'password'),
], 'Welcome\nNext', 'https://accounts.google.com/v3/signin/challenge/pwd');
assert.equal(vm.runInContext(inspect, password).action, 'password');
vm.runInContext(helpers, password);
assert.equal(vm.runInContext("cpaFindInput('input[type=password]', 'password')", password).id, 'passwordField');

const terms = makeContext([], 'I understand', 'https://accounts.google.com/speedbump/gaplustos');
assert.equal(vm.runInContext(inspect, terms).action, 'consent');

const network = makeContext([], 'This site cannot be reached\nERR_CONNECTION_RESET', 'chrome-error://chromewebdata/');
const failure = vm.runInContext(inspect, network);
assert.equal(failure.action, 'network_error');
assert.equal(failure.network_code, 'ERR_CONNECTION_RESET');
console.log('Google identifier, visible password, Workspace consent, and network fixtures passed');
