const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

class Element {
  constructor() {
    this.listeners = new Map();
    this.classList = { add() {}, remove() {}, toggle() {} };
    this.value = '';
    this.textContent = '';
  }
  addEventListener(name, callback) { this.listeners.set(name, callback); }
  replaceChildren() {}
  append() {}
  querySelector() { return null; }
}

test('new remote runs retain their session for streamed events', () => {
  const elements = new Map();
  const document = {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, new Element());
      return elements.get(id);
    },
    createElement() { return new Element(); },
  };
  const sent = [];
  const context = {
    document,
    window: { addEventListener() {} },
    location: { protocol: 'https:', host: 'stable.test' },
    navigator: { userAgent: 'Test browser' },
    crypto: { randomUUID: () => 'request-1' },
    WebSocket: { OPEN: 1 },
    fetch: () => new Promise(() => {}),
    setInterval,
    clearInterval,
    console,
  };
  const source = fs.readFileSync('internal/remote/ui/app.js', 'utf8') +
    '\nglobalThis.__state = state; globalThis.__submit = $("message-form").listeners.get("submit");';
  vm.runInNewContext(source, context);
  const state = context.__state;
  state.connected = true;
  state.session = 'session-1';
  state.socket = { readyState: 1, send: (message) => sent.push(JSON.parse(message)) };
  document.getElementById('message').value = 'hello';
  context.__submit({ preventDefault() {} });
  const request = sent[0];
  assert.equal(request.message.op, 'run_start');
  assert.deepEqual({ ...state.pending.get(request.id) }, { op: 'run_start', session: 'session-1', run: true });
});
