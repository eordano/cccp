import { writeFile } from 'node:fs/promises';

export async function connect(port = process.env.CDP_PORT || 19222) {
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const target = targets.find(t => t.type === 'page');
  if (!target) throw new Error('No Chromium page found');
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject; });
  let id = 0;
  const pending = new Map();
  const errors = [];
  const listeners = new Map();
  ws.onmessage = event => {
    const message = JSON.parse(event.data);
    const listener = listeners.get(message.method);
    if (listener) { listeners.delete(message.method); listener(message.params); }
    if (message.method === 'Runtime.exceptionThrown') errors.push(message.params.exceptionDetails);
    const task = pending.get(message.id);
    if (!task) return;
    pending.delete(message.id);
    message.error ? task.reject(new Error(JSON.stringify(message.error))) : task.resolve(message.result);
  };
  function send(method, params = {}) {
    return new Promise((resolve, reject) => {
      const key = ++id;
      pending.set(key, { resolve, reject });
      ws.send(JSON.stringify({ id: key, method, params }));
    });
  }
  async function evaluate(expression) {
    const result = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true }).catch(error => { throw new Error(expression.slice(0,180) + ': ' + error.message); });
    if (result.exceptionDetails) throw new Error(JSON.stringify(result.exceptionDetails));
    return result.result.value;
  }
  async function wait(expression) {
    return evaluate(`new Promise((resolve,reject)=>{const end=Date.now()+10000; const check=()=>{if(${expression})resolve(true);else if(Date.now()>end)reject(new Error('Timed out waiting for UI'));else requestAnimationFrame(check)};check()})`);
  }
  async function loadedAfter(action) {
    const loaded = new Promise(resolve => listeners.set("Page.loadEventFired", resolve));
    await action();
    await loaded;
  }
  async function navigate(url) {
    const loaded = new Promise(resolve => listeners.set('Page.loadEventFired', resolve));
    await send('Page.navigate', { url });
    await loaded;
  }
  async function screenshot(path) {
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    await writeFile(path, Buffer.from(shot.data, 'base64'));
  }
  await send('Runtime.enable');
  await send('Page.enable');
  await send('Page.bringToFront');
  return { send, evaluate, wait, navigate, loadedAfter, screenshot, errors, close: () => ws.close() };
}
