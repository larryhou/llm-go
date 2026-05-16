package main

// uiHTML is the embedded single-page web UI served at /.
// It communicates with the backend via SSE POST /chat.
const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Control</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  :root {
    --bg: #1e1e2e; --surface: #313244; --overlay: #45475a;
    --text: #cdd6f4; --subtext: #a6adc8; --green: #a6e3a1;
    --yellow: #f9e2af; --red: #f38ba8; --blue: #89b4fa;
    --mauve: #cba6f7; --teal: #94e2d5;
    --font: "JetBrains Mono", "Fira Code", monospace;
  }
  body { background: var(--bg); color: var(--text); font-family: var(--font); font-size: 14px; height: 100vh; display: flex; flex-direction: column; }

  /* header */
  header { padding: 10px 16px; background: var(--surface); border-bottom: 1px solid var(--overlay); display: flex; align-items: center; gap: 12px; flex-shrink: 0; }
  header h1 { font-size: 15px; color: var(--mauve); }
  #token-bar { margin-left: auto; display: flex; gap: 16px; font-size: 12px; color: var(--subtext); align-items: center; }
  #token-bar span { display: flex; align-items: center; gap: 4px; }
  #token-bar .val { color: var(--teal); font-weight: bold; }
  #ctx-btn  { background: var(--overlay); color: var(--subtext); border: none; border-radius: 6px; padding: 4px 10px; font-family: var(--font); font-size: 12px; cursor: pointer; }
  #ctx-btn:hover  { color: var(--text); }
  #rec-btn  { background: var(--overlay); color: var(--subtext); border: none; border-radius: 6px; padding: 4px 10px; font-family: var(--font); font-size: 12px; cursor: pointer; }
  #rec-btn.active { background: var(--red); color: var(--bg); }
  #rec-btn:hover  { color: var(--text); }

  /* messages */
  #messages { flex: 1; overflow-y: auto; padding: 16px; display: flex; flex-direction: column; gap: 12px; }
  .msg { display: flex; flex-direction: column; gap: 4px; max-width: 90%; }
  .msg.user { align-self: flex-end; }
  .msg.assistant { align-self: flex-start; }
  .bubble { padding: 10px 14px; border-radius: 12px; white-space: pre-wrap; word-break: break-word; line-height: 1.6; }
  .msg.user .bubble { background: var(--mauve); color: var(--bg); border-bottom-right-radius: 3px; }
  .msg.assistant .bubble { background: var(--surface); border-bottom-left-radius: 3px; }

  /* tool calls */
  .tool-group { display: flex; flex-direction: column; gap: 2px; align-self: flex-start; }
  .tool-latest { font-size: 11px; color: var(--yellow); background: var(--overlay); padding: 2px 8px; border-radius: 4px; cursor: default; user-select: none; }
  .tool-history { display: none; flex-direction: column; gap: 2px; }
  .tool-history.open { display: flex; }
  .tool-history-item { font-size: 11px; color: var(--subtext); background: var(--overlay); padding: 2px 8px; border-radius: 4px; }
  .tool-toggle { font-size: 10px; color: var(--subtext); cursor: pointer; padding: 1px 6px; border-radius: 3px; align-self: flex-start; }
  .tool-toggle:hover { color: var(--text); }
  .cancelled-tag { font-size: 12px; color: var(--subtext); font-style: italic; margin-top: 2px; }
  .error-tag { font-size: 12px; color: var(--red); }
  .cursor { display: inline-block; width: 8px; height: 14px; background: var(--teal); vertical-align: text-bottom; animation: blink .8s step-end infinite; }
  @keyframes blink { 50% { opacity: 0; } }

  /* input area */
  footer { padding: 12px 16px; background: var(--surface); border-top: 1px solid var(--overlay); display: flex; gap: 8px; flex-shrink: 0; }
  #input { flex: 1; background: var(--bg); border: 1px solid var(--overlay); color: var(--text); padding: 10px 12px; border-radius: 8px; font-family: var(--font); font-size: 14px; resize: none; height: 44px; max-height: 200px; overflow-y: auto; outline: none; }
  #input:focus { border-color: var(--mauve); }
  #send { background: var(--mauve); color: var(--bg); border: none; border-radius: 8px; padding: 0 18px; font-family: var(--font); font-size: 14px; font-weight: bold; cursor: pointer; }
  #send:disabled { opacity: .4; cursor: not-allowed; }

  /* scrollbar */
  #messages::-webkit-scrollbar { width: 6px; }
  #messages::-webkit-scrollbar-track { background: transparent; }
  #messages::-webkit-scrollbar-thumb { background: var(--overlay); border-radius: 3px; }

  /* context panel */
  #ctx-panel { display: none; position: fixed; top: 0; right: 0; width: 600px; height: 100vh; background: var(--surface); border-left: 1px solid var(--overlay); z-index: 100; flex-direction: column; overflow: hidden; }
  #ctx-panel.open { display: flex; }
  #ctx-head { padding: 10px 14px; background: var(--overlay); display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
  #ctx-head h2 { font-size: 13px; color: var(--mauve); flex: 1; margin: 0; }
  #ctx-refresh { background: none; border: 1px solid var(--subtext); color: var(--subtext); border-radius: 4px; padding: 2px 8px; font-family: var(--font); font-size: 11px; cursor: pointer; }
  #ctx-refresh:hover { color: var(--text); border-color: var(--text); }
  #ctx-close { background: none; border: none; color: var(--subtext); cursor: pointer; font-size: 18px; line-height: 1; padding: 0 4px; }
  #ctx-close:hover { color: var(--text); }
  #ctx-meta { padding: 6px 14px; font-size: 11px; color: var(--subtext); border-bottom: 1px solid var(--overlay); flex-shrink: 0; }
  #ctx-body { flex: 1; min-height: 0; overflow-y: auto; padding: 10px 14px; display: flex; flex-direction: column; gap: 6px; }
  #ctx-body::-webkit-scrollbar { width: 6px; }
  #ctx-body::-webkit-scrollbar-track { background: transparent; }
  #ctx-body::-webkit-scrollbar-thumb { background: var(--overlay); border-radius: 3px; }
  .ctx-msg { border: 1px solid var(--overlay); border-radius: 6px; overflow: hidden; flex-shrink: 0; }
  .ctx-msg-head { padding: 6px 10px; font-size: 11px; display: flex; gap: 8px; align-items: center; background: var(--bg); cursor: pointer; user-select: none; }
  .ctx-msg-head:hover { background: var(--overlay); }
  .ctx-role { font-weight: bold; }
  .ctx-role.user { color: var(--mauve); }
  .ctx-role.assistant { color: var(--green); }
  .ctx-role.tool { color: var(--yellow); }
  .ctx-chars { color: var(--subtext); margin-left: auto; font-size: 10px; }
  .ctx-toggle-icon { color: var(--subtext); font-size: 10px; transition: transform .15s; }
  .ctx-parts { padding: 8px 10px; display: flex; flex-direction: column; gap: 8px; border-top: 1px solid var(--overlay); }
  .ctx-part { border: 1px solid var(--overlay); border-radius: 4px; overflow: hidden; }
  .ctx-part-type { font-size: 10px; color: var(--teal); padding: 3px 8px; background: var(--bg); cursor: pointer; user-select: none; display: flex; justify-content: space-between; align-items: center; }
  .ctx-part-type:hover { background: var(--overlay); }
  .ctx-part-preview { font-size: 11px; color: var(--subtext); white-space: pre-wrap; word-break: break-all; padding: 6px 8px; max-height: 300px; overflow-y: auto; display: none; }
  .ctx-part-preview.open { display: block; }
  .ctx-part-preview::-webkit-scrollbar { width: 4px; }
  .ctx-part-preview::-webkit-scrollbar-thumb { background: var(--overlay); border-radius: 2px; }
</style>
</head>
<body>
<header>
  <h1>Control</h1>
  <div id="token-bar">
    <span>in <span class="val" id="tok-in">0</span></span>
    <span>out <span class="val" id="tok-out">0</span></span>
    <span>total <span class="val" id="tok-total">0</span></span>
    <button id="ctx-btn">Context</button>
    <button id="rec-btn">Record</button>
  </div>
</header>

<div id="messages"></div>

<footer>
  <textarea id="input" placeholder="Ask anything…" rows="1"></textarea>
  <button id="send">Send</button>
</footer>

<!-- context panel -->
<div id="ctx-panel">
  <div id="ctx-head">
    <h2>Context Window</h2>
    <button id="ctx-refresh">Refresh</button>
    <button id="ctx-close">×</button>
  </div>
  <div id="ctx-meta" id="ctx-meta"></div>
  <div id="ctx-body"></div>
</div>

<script>
const messages  = document.getElementById('messages');
const input     = document.getElementById('input');
const sendBtn   = document.getElementById('send');
const tokIn     = document.getElementById('tok-in');
const tokOut    = document.getElementById('tok-out');
const tokTotal  = document.getElementById('tok-total');
const ctxBtn    = document.getElementById('ctx-btn');
const ctxPanel  = document.getElementById('ctx-panel');
const ctxClose  = document.getElementById('ctx-close');
const ctxRefresh= document.getElementById('ctx-refresh');
const ctxMeta   = document.getElementById('ctx-meta');
const ctxBody   = document.getElementById('ctx-body');
const recBtn    = document.getElementById('rec-btn');

// ── turn state ────────────────────────────────────────────────────────────────

let turnActive = false;

function setTurnActive(active) {
  turnActive = active;
}

async function cancelTurn() {
  if (!turnActive) return;
  try { await fetch('/cancel', { method: 'POST' }); } catch (_) {}
}

// ESC cancels the active turn (anywhere on the page).
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && turnActive) {
    e.preventDefault();
    cancelTurn();
  }
});

// ── recording ────────────────────────────────────────────────────────────────

let recording = false;
let recordLog  = []; // array of { ts, type, data }

function recEvent(type, data) {
  if (!recording) return;
  recordLog.push({ ts: Date.now(), type, data });
}

function downloadRecording() {
  if (recordLog.length === 0) return;
  const blob = new Blob([JSON.stringify(recordLog, null, 2)], { type: 'application/json' });
  const url  = URL.createObjectURL(blob);
  const a    = document.createElement('a');
  a.href     = url;
  a.download = 'control-session-' + Date.now() + '.json';
  a.click();
  URL.revokeObjectURL(url);
}

recBtn.addEventListener('click', () => {
  if (!recording) {
    recording  = true;
    recordLog  = [];
    recBtn.textContent = 'Stop & Save';
    recBtn.classList.add('active');
    recEvent('record_start', { time: new Date().toISOString() });
  } else {
    recEvent('record_stop', { time: new Date().toISOString() });
    recording = false;
    recBtn.textContent = 'Record';
    recBtn.classList.remove('active');
    downloadRecording();
  }
});

// ── context panel ────────────────────────────────────────────────────────────

async function loadContext() {
  ctxMeta.textContent = 'Loading…';
  ctxBody.innerHTML   = '';
  try {
    const resp = await fetch('/context');
    const data = await resp.json();
    if (data.error) { ctxMeta.textContent = 'Error: ' + data.error; return; }

    ctxMeta.textContent =
      data.messages + ' messages · ' + data.total_chars.toLocaleString() + ' chars';

    for (const msg of (data.context || [])) {
      const box = document.createElement('div');
      box.className = 'ctx-msg';

      const head = document.createElement('div');
      head.className = 'ctx-msg-head';

      const role = document.createElement('span');
      role.className = 'ctx-role ' + msg.role;
      role.textContent = msg.role;

      const chars = document.createElement('span');
      chars.className = 'ctx-chars';
      chars.textContent = msg.total_chars.toLocaleString() + ' chars';

      const icon = document.createElement('span');
      icon.className = 'ctx-toggle-icon';
      icon.textContent = '▶';

      head.appendChild(role);
      head.appendChild(chars);
      head.appendChild(icon);
      box.appendChild(head);

      const partsEl = document.createElement('div');
      partsEl.className = 'ctx-parts';
      partsEl.style.display = 'none';

      for (const p of (msg.content || [])) {
        const item = document.createElement('div');
        item.className = 'ctx-part';

        const typeEl = document.createElement('div');
        typeEl.className = 'ctx-part-type';
        const typeLabel = document.createElement('span');
        typeLabel.textContent = p.type + ' · ' + p.chars.toLocaleString() + ' chars';
        const typeIcon = document.createElement('span');
        typeIcon.textContent = '▶';
        typeIcon.style.fontSize = '9px';
        typeEl.appendChild(typeLabel);
        typeEl.appendChild(typeIcon);
        item.appendChild(typeEl);

        if (p.preview) {
          const prev = document.createElement('div');
          prev.className = 'ctx-part-preview';
          prev.textContent = p.preview;
          item.appendChild(prev);

          typeEl.addEventListener('click', () => {
            const open = prev.classList.toggle('open');
            typeIcon.textContent = open ? '▼' : '▶';
          });
        }
        partsEl.appendChild(item);
      }
      box.appendChild(partsEl);

      head.addEventListener('click', () => {
        const open = partsEl.style.display !== 'none';
        partsEl.style.display = open ? 'none' : 'flex';
        icon.textContent = open ? '▶' : '▼';
      });

      ctxBody.appendChild(box);
    }
  } catch(e) {
    ctxMeta.textContent = 'Error: ' + e.message;
  }
}

ctxBtn.addEventListener('click', () => {
  ctxPanel.classList.toggle('open');
  if (ctxPanel.classList.contains('open')) loadContext();
});
ctxClose.addEventListener('click', () => ctxPanel.classList.remove('open'));
ctxRefresh.addEventListener('click', loadContext);

// ── chat ─────────────────────────────────────────────────────────────────────

function scrollBottom() {
  messages.scrollTop = messages.scrollHeight;
}

function addUserMsg(text) {
  const d = document.createElement('div');
  d.className = 'msg user';
  d.innerHTML = '<div class="bubble">' + escHtml(text) + '</div>';
  messages.appendChild(d);
  scrollBottom();
}

function startAssistantMsg() {
  const wrap = document.createElement('div');
  wrap.className = 'msg assistant';
  const bubble = document.createElement('div');
  bubble.className = 'bubble';
  const cursor = document.createElement('span');
  cursor.className = 'cursor';
  bubble.appendChild(cursor);
  wrap.appendChild(bubble);
  messages.appendChild(wrap);
  scrollBottom();
  return { wrap, bubble, cursor };
}

function makeTurnTools(wrap) {
  const calls = [];
  let group = null, historyEl = null, toggleEl = null, latestEl = null;

  function rebuild() {
    const n = calls.length;
    if (n === 0) return;
    if (!group) {
      group     = document.createElement('div'); group.className = 'tool-group';
      historyEl = document.createElement('div'); historyEl.className = 'tool-history';
      toggleEl  = document.createElement('span'); toggleEl.className = 'tool-toggle';
      latestEl  = document.createElement('div'); latestEl.className = 'tool-latest';
      group.appendChild(historyEl);
      group.appendChild(toggleEl);
      group.appendChild(latestEl);
      wrap.appendChild(group);
    }
    latestEl.textContent = '⚙ ' + calls[n - 1];
    historyEl.innerHTML  = '';
    for (let i = 0; i < n - 1; i++) {
      const item = document.createElement('div');
      item.className = 'tool-history-item';
      item.textContent = '⚙ ' + calls[i];
      historyEl.appendChild(item);
    }
    const hidden = n - 1;
    if (hidden <= 0) {
      toggleEl.style.display = 'none';
    } else {
      toggleEl.style.display = '';
      const isOpen = historyEl.classList.contains('open');
      toggleEl.textContent = isOpen ? '▲ hide' : '▼ +' + hidden + ' more';
      toggleEl.onclick = () => {
        const nowOpen = historyEl.classList.toggle('open');
        toggleEl.textContent = nowOpen ? '▲ hide' : '▼ +' + hidden + ' more';
        scrollBottom();
      };
    }
    scrollBottom();
  }

  return { add(label) { calls.push(label); rebuild(); } };
}

function appendError(wrap, msg) {
  const tag = document.createElement('div');
  tag.className = 'error-tag';
  tag.textContent = '✗ ' + msg;
  wrap.appendChild(tag);
  scrollBottom();
}

function appendCancelled(wrap) {
  const tag = document.createElement('div');
  tag.className = 'cancelled-tag';
  tag.textContent = '⊘ cancelled';
  wrap.appendChild(tag);
  scrollBottom();
}

function escHtml(s) {
  return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

async function send() {
  const text = input.value.trim();
  if (!text) return;
  input.value = '';
  input.style.height = '44px';
  sendBtn.disabled = true;
  setTurnActive(true);

  recEvent('user_message', { text });
  addUserMsg(text);
  const { wrap, bubble, cursor } = startAssistantMsg();
  let textNode = document.createTextNode('');
  bubble.insertBefore(textNode, cursor);
  const turnTools = makeTurnTools(wrap);

  let assistantText = '';
  let wasCancelled  = false;
  const turnStart = Date.now();

  try {
    const resp = await fetch('/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: text }),
    });

    const reader  = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      const parts = buf.split('\n\n');
      buf = parts.pop();
      for (const part of parts) {
        if (!part.startsWith('data: ')) continue;
        let ev;
        try { ev = JSON.parse(part.slice(6)); } catch { continue; }

        if (ev.type === 'text') {
          textNode.textContent += ev.delta;
          assistantText += ev.delta;
          scrollBottom();
        } else if (ev.type === 'tool_call') {
          const label = ev.input ? ev.tool + ' ' + ev.input : ev.tool;
          turnTools.add(label);
          recEvent('tool_call', { tool: ev.tool, input: ev.input });
          textNode = document.createTextNode('');
          bubble.insertBefore(textNode, cursor);
        } else if (ev.type === 'usage') {
          tokIn.textContent    = ev.input.toLocaleString();
          tokOut.textContent   = ev.output.toLocaleString();
          tokTotal.textContent = ev.total.toLocaleString();
          recEvent('usage', { input: ev.input, output: ev.output, total: ev.total });
        } else if (ev.type === 'error') {
          // context.Canceled from a user-triggered cancel is not an error.
          if (ev.error && ev.error.includes('context canceled')) {
            wasCancelled = true;
          } else {
            appendError(wrap, ev.error);
            recEvent('error', { error: ev.error });
          }
        } else if (ev.type === 'done') {
          recEvent('assistant_message', {
            text: assistantText,
            duration_ms: Date.now() - turnStart,
            input_tokens: parseInt(tokIn.textContent.replace(/,/g,''), 10) || 0,
          });
          // Auto-refresh context panel if open
          if (ctxPanel.classList.contains('open')) loadContext();
        } else if (ev.type === 'cancelled') {
          wasCancelled = true;
        }
      }
    }
  } catch (e) {
    // AbortError or network close after cancel — treat as cancelled.
    if (e.name === 'AbortError') {
      wasCancelled = true;
    } else {
      appendError(wrap, e.message);
      recEvent('error', { error: e.message });
    }
  }

  cursor.remove();
  if (wasCancelled) appendCancelled(wrap);
  scrollBottom();
  setTurnActive(false);
  sendBtn.disabled = false;
  input.focus();
}

sendBtn.addEventListener('click', send);
input.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
});
input.addEventListener('input', () => {
  input.style.height = '44px';
  input.style.height = Math.min(input.scrollHeight, 200) + 'px';
});
</script>
</body>
</html>`
