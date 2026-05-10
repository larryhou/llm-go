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
  header { padding: 10px 16px; background: var(--surface); border-bottom: 1px solid var(--overlay); display: flex; align-items: center; gap: 12px; }
  header h1 { font-size: 15px; color: var(--mauve); }
  #token-bar { margin-left: auto; display: flex; gap: 16px; font-size: 12px; color: var(--subtext); }
  #token-bar span { display: flex; align-items: center; gap: 4px; }
  #token-bar .val { color: var(--teal); font-weight: bold; }

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
  .error-tag { font-size: 12px; color: var(--red); }
  .cursor { display: inline-block; width: 8px; height: 14px; background: var(--teal); vertical-align: text-bottom; animation: blink .8s step-end infinite; }
  @keyframes blink { 50% { opacity: 0; } }

  /* input area */
  footer { padding: 12px 16px; background: var(--surface); border-top: 1px solid var(--overlay); display: flex; gap: 8px; }
  #input { flex: 1; background: var(--bg); border: 1px solid var(--overlay); color: var(--text); padding: 10px 12px; border-radius: 8px; font-family: var(--font); font-size: 14px; resize: none; height: 44px; max-height: 200px; overflow-y: auto; outline: none; }
  #input:focus { border-color: var(--mauve); }
  #send { background: var(--mauve); color: var(--bg); border: none; border-radius: 8px; padding: 0 18px; font-family: var(--font); font-size: 14px; font-weight: bold; cursor: pointer; }
  #send:disabled { opacity: .4; cursor: not-allowed; }

  /* scrollbar */
  #messages::-webkit-scrollbar { width: 6px; }
  #messages::-webkit-scrollbar-track { background: transparent; }
  #messages::-webkit-scrollbar-thumb { background: var(--overlay); border-radius: 3px; }
</style>
</head>
<body>
<header>
  <h1>⌨ Control</h1>
  <div id="token-bar">
    <span>in <span class="val" id="tok-in">0</span></span>
    <span>out <span class="val" id="tok-out">0</span></span>
    <span>total <span class="val" id="tok-total">0</span></span>
  </div>
</header>

<div id="messages"></div>

<footer>
  <textarea id="input" placeholder="Ask anything…" rows="1"></textarea>
  <button id="send">Send</button>
</footer>

<script>
const messages = document.getElementById('messages');
const input    = document.getElementById('input');
const sendBtn  = document.getElementById('send');
const tokIn    = document.getElementById('tok-in');
const tokOut   = document.getElementById('tok-out');
const tokTotal = document.getElementById('tok-total');

let sessionId = '';


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

// toolState tracks the collapsible tool-call group for one assistant turn.
// structure inside wrap:
//   .tool-group
//     .tool-history   (hidden by default, expands on toggle click)
//       .tool-history-item  × N-1  (older calls)
//     .tool-toggle    ("+N more" / "hide")
//     .tool-latest    (always visible — most recent call)
function makeTurnTools(wrap) {
  const calls = [];          // all label strings so far
  let group = null;          // .tool-group node, created on first call
  let historyEl = null;      // .tool-history node
  let toggleEl = null;       // .tool-toggle node
  let latestEl = null;       // .tool-latest node

  function rebuild() {
    const n = calls.length;
    if (n === 0) return;

    // Create group on first call
    if (!group) {
      group = document.createElement('div');
      group.className = 'tool-group';
      historyEl = document.createElement('div');
      historyEl.className = 'tool-history';
      toggleEl = document.createElement('span');
      toggleEl.className = 'tool-toggle';
      latestEl = document.createElement('div');
      latestEl.className = 'tool-latest';
      group.appendChild(historyEl);
      group.appendChild(toggleEl);
      group.appendChild(latestEl);
      wrap.appendChild(group);
    }

    // Latest is always the last call
    latestEl.textContent = '⚙ ' + calls[n - 1];

    // History = all but the last
    historyEl.innerHTML = '';
    for (let i = 0; i < n - 1; i++) {
      const item = document.createElement('div');
      item.className = 'tool-history-item';
      item.textContent = '⚙ ' + calls[i];
      historyEl.appendChild(item);
    }

    // Toggle button
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

  return {
    add(label) { calls.push(label); rebuild(); }
  };
}

function appendError(wrap, msg) {
  const tag = document.createElement('div');
  tag.className = 'error-tag';
  tag.textContent = '✗ ' + msg;
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

  addUserMsg(text);
  const { wrap, bubble, cursor } = startAssistantMsg();
  let textNode = document.createTextNode('');
  bubble.insertBefore(textNode, cursor);
  const turnTools = makeTurnTools(wrap);

  try {
    const resp = await fetch('/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: text }),
    });

    const reader = resp.body.getReader();
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
          scrollBottom();
        } else if (ev.type === 'tool_call') {
          const label = ev.input ? ev.tool + ' ' + ev.input : ev.tool;
          turnTools.add(label);
          // start fresh text node after tool group
          textNode = document.createTextNode('');
          bubble.insertBefore(textNode, cursor);
        } else if (ev.type === 'usage') {
          if (ev.input  > 0) tokIn.textContent    = ev.input.toLocaleString();
          if (ev.output > 0) tokOut.textContent   = ev.output.toLocaleString();
          if (ev.total  > 0) tokTotal.textContent = ev.total.toLocaleString();
        } else if (ev.type === 'error') {
          appendError(wrap, ev.error);
        } else if (ev.type === 'done') {
          // session managed server-side
        }
      }
    }
  } catch (e) {
    appendError(wrap, e.message);
  }

  cursor.remove();
  scrollBottom();
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
