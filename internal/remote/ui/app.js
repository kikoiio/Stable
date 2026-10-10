const $ = (id) => document.getElementById(id);
const state = { socket: null, connected: false, grant: null, root: "", session: null, sessions: [], runID: "", runRequestID: "", pending: new Map(), timer: null, approvals: [], questions: [], plans: [] };
const status = (text) => { $("status").textContent = text || ""; };
const setConnection = (text, live = false) => { const el = $("connection"); el.textContent = text; el.classList.toggle("live", live); };
const newID = () => crypto.randomUUID();

fetch("/api/session", { cache: "no-store" }).then((response) => {
  if (response.ok) { $("pair-panel").classList.add("hidden"); $("workspace").classList.remove("hidden"); setConnection("Paired · offline"); }
}).catch(() => {});

$("pair-form").addEventListener("submit", async (event) => {
  event.preventDefault(); status("");
  const token = $("pair-token").value.trim();
  try {
    const response = await fetch("/api/pair", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token }) });
    $("pair-token").value = "";
    if (!response.ok) throw new Error("Pairing failed. Check the token and try again.");
    $("pair-panel").classList.add("hidden"); $("workspace").classList.remove("hidden"); setConnection("Paired · offline"); status("Device paired. Enter a project path and request local approval.");
  } catch (error) { status(error.message); }
});

$("connect").addEventListener("click", () => connectProject());
$("refresh").addEventListener("click", () => request("session_list", { project_root: state.root }));
$("new-session").addEventListener("click", () => request("session_create", { project_root: state.root }));
$("message-form").addEventListener("submit", (event) => {
  event.preventDefault(); const text = $("message").value.trim();
  if (!text || !state.session) return;
  $("message").value = ""; addBubble("user", text);
  const session = state.session;
  const id = request("run_start", { session_id: session, run: { work: { kind: "session", session_id: session }, intent: text, messages: [{ role: "user", content: text }] } });
  state.runRequestID = id;
  if (id) state.pending.set(id, { op: "run_start", session, run: true });
  $("run-status").textContent = "Starting…";
});
$("cancel-run").addEventListener("click", () => {
  if (!state.session || !state.runID) return;
  request("run_cancel", { session_id: state.session, run_id: state.runID });
});

function connectProject() {
  const root = $("project-root").value.trim();
  if (!root) { status("Enter an absolute project directory path."); return; }
  if (state.socket) state.socket.close();
  state.root = root; state.session = null; state.grant = null; state.connected = false; state.runID = ""; state.runRequestID = ""; state.approvals = []; state.questions = []; state.plans = []; state.pending.clear();
  setConnection("Waiting for local approval"); status("Approve this project directory in the local TUI.");
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${scheme}//${location.host}/ws`); state.socket = socket;
  socket.addEventListener("open", () => socket.send(JSON.stringify({ id: newID(), message: { op: "remote_access_request", project_root: root, remote_client_label: navigator.userAgent.includes("Mobile") ? "Mobile browser" : "Browser" } })));
  socket.addEventListener("message", (event) => {
    let envelope; try { envelope = JSON.parse(event.data); } catch { status("The remote service sent invalid data."); return; }
    handleMessage(envelope.id, envelope.message || {});
  });
  socket.addEventListener("close", () => {
    state.connected = false; state.grant = null; state.session = null; state.runID = "";
    $("message").disabled = true; $("send").disabled = true; $("cancel-run").classList.add("hidden");
    if (state.timer) clearInterval(state.timer); setConnection("Disconnected");
  });
  socket.addEventListener("error", () => status("Remote connection failed. Check the service and try again."));
}

function request(op, fields = {}) {
  if (!state.socket || state.socket.readyState !== WebSocket.OPEN || !state.connected) { status("Connect to an approved project first."); return ""; }
  const id = newID(); state.pending.set(id, { op, session: fields.session_id || "" });
  state.socket.send(JSON.stringify({ id, message: { op, ...fields } })); return id;
}

function handleMessage(id, msg) {
  if (msg.type === "error") { status(msg.error || "Request failed."); if (state.pending.get(id)?.run) $("run-status").textContent = "Run failed"; }
  if (msg.type === "remote_grant") {
    state.grant = msg.remote_grant; state.connected = true; state.root = state.grant.project_root;
    $("root-label").textContent = state.root; setConnection("Connected · directory approved", true); status("");
    $("message").disabled = false; $("send").disabled = false;
    request("session_list", { project_root: state.root });
    if (state.timer) clearInterval(state.timer);
    state.timer = setInterval(() => {
      if (state.session) { request("approval_list", { session_id: state.session }); request("question_list", { session_id: state.session }); }
      request("session_list", { project_root: state.root });
    }, 10000);
  }
  if (msg.sessions) renderSessions(msg.sessions);
  if (msg.session) { state.session = msg.session.id; $("session-title").textContent = msg.session.title || "Session"; $("message").disabled = false; $("send").disabled = false; request("session_load", { project_root: state.root, session_id: state.session }); request("session_list", { project_root: state.root }); }
  if (msg.transcript) { if (msg.transcript.session?.title) $("session-title").textContent = msg.transcript.session.title; renderTranscript(msg.transcript); }
  if (msg.goals) renderGoals(msg.goals, msg.goal_events || []);
  if (msg.plan) $("run-status").textContent = `Plan mode · ${msg.plan.mode}`;
  if (msg.approvals) { state.approvals = msg.approvals; renderRequests(); }
  if (msg.questions) { state.questions = msg.questions; renderRequests(); }
  if (msg.plan_approvals) { state.plans = msg.plan_approvals; renderRequests(); }
  if (msg.plan_state) $("run-status").textContent = `Plan mode · ${msg.plan_state.mode}`;
  const requestInfo = state.pending.get(id);
  if (msg.run_event && requestInfo?.session === state.session) renderRunEvent(msg.run_event);
  if (msg.type === "run_started" && id === state.runRequestID) { state.runID = msg.run_id; $("run-status").textContent = "Running"; $("cancel-run").classList.remove("hidden"); }
  if (msg.type === "run_outcome") {
    state.pending.delete(id);
    if (id === state.runRequestID) { state.runID = ""; state.runRequestID = ""; $("run-status").textContent = msg.outcome?.status || "Finished"; $("cancel-run").classList.add("hidden"); }
  }
  if (msg.type === "done") {
    const pending = state.pending.get(id); state.pending.delete(id);
    if (pending?.op === "run_start" && state.runID === "") { $("run-status").textContent = "Finished"; $("cancel-run").classList.add("hidden"); }
  }
}

function renderSessions(sessions) {
  state.sessions = sessions; const list = $("sessions"); list.replaceChildren();
  for (const session of sessions) {
    const button = document.createElement("button"); button.className = "session-item" + (session.id === state.session ? " selected" : "");
    button.textContent = session.title || session.id; button.addEventListener("click", () => selectSession(session.id)); list.append(button);
  }
  if (!sessions.length) list.innerHTML = '<p class="hint">No sessions yet. Start a new session.</p>';
}

function selectSession(id) {
  state.session = id; state.runID = ""; state.runRequestID = ""; $("run-status").textContent = ""; $("cancel-run").classList.add("hidden"); $("session-title").textContent = "Loading session…";
  request("session_load", { project_root: state.root, session_id: id });
  request("approval_list", { session_id: id }); request("question_list", { session_id: id });
  renderSessions(state.sessions);
}

function renderTranscript(transcript) {
  const container = $("transcript"); container.replaceChildren();
  const events = transcript.events || transcript.Events || [];
  const runs = new Map();
  const runBubbles = new Map();
  for (const event of events) {
    const kind = event.type || event.Type; const data = event.data || event.Data || {};
    if (kind === "message") addBubble((data.role || "assistant") === "user" ? "user" : "assistant", data.text || "", container);
    if (kind === "run_started" && data.run_id) runs.set(data.run_id, { lastSeq: event.seq || 0, terminal: false });
    if (kind === "run_event") {
      const runID = data.run_id || data.RunID; const run = runs.get(runID);
      if (run) { run.lastSeq = event.seq || run.lastSeq; if ((data.kind || data.Kind) === "terminal") run.terminal = true; }
      const payload = data.payload || data.Payload || {};
      let decoded = payload; if (typeof payload === "string") { try { decoded = JSON.parse(payload); } catch {} }
      if ((data.kind || data.Kind) === "text_delta" && (decoded.text || decoded.content)) {
        let content = runBubbles.get(data.run_id || data.RunID);
        if (!content) { content = addBubble("assistant", "", container); runBubbles.set(data.run_id || data.RunID, content); }
        content.textContent += decoded.text || decoded.content;
      }
    }
  }
  if (!container.children.length) container.innerHTML = '<p class="empty">This session has no messages yet.</p>';
  container.scrollTop = container.scrollHeight;
  const running = [...runs.entries()].reverse().find(([, run]) => !run.terminal);
  if (running && state.session) {
    state.runID = running[0]; $("run-status").textContent = "Reconnecting to run…"; $("cancel-run").classList.remove("hidden");
    state.runRequestID = request("run_subscribe", { session_id: state.session, run_id: running[0], after_seq: running[1].lastSeq });
  }
}

function addBubble(role, text, container = $("transcript")) {
  const bubble = document.createElement("article"); bubble.className = `bubble ${role}`;
  const label = document.createElement("div"); label.className = "bubble-label"; label.textContent = role === "user" ? "You" : "Stable";
  const content = document.createElement("div"); content.textContent = text;
  bubble.append(label, content); container.append(bubble); container.scrollTop = container.scrollHeight; return content;
}

function renderRunEvent(event) {
  const kind = event.kind || event.Kind; const payload = event.payload || event.Payload;
  if (!payload) return;
  let decoded = payload; if (typeof payload === "string") { try { decoded = JSON.parse(payload); } catch {} }
  const text = decoded.text || decoded.content || decoded.message || "";
  if (kind === "text_delta" && text) {
    let bubble = $("transcript").lastElementChild;
    if (!bubble || !bubble.classList.contains("assistant")) { addBubble("assistant", ""); bubble = $("transcript").lastElementChild; }
    bubble.lastElementChild.textContent += text;
  }
  if (kind === "terminal") { $("run-status").textContent = decoded.status || "Finished"; }
}

function renderGoals(goals, events = []) {
  const list = $("goals"); list.replaceChildren();
  for (const goal of goals) {
    const row = document.createElement("div"); row.className = "goal";
    const title = document.createElement("strong"); title.textContent = goal.objective || goal.title || goal.id || "Goal";
    const detail = document.createElement("span"); detail.textContent = [goal.status, goal.reason, goal.evidence_summary].filter(Boolean).join(" · ");
    row.append(title, detail);
    const recent = events.filter((event) => event.goal_id === goal.id).slice(0, 3);
    if (recent.length) {
      const history = document.createElement("span"); history.className = "goal-events";
      history.textContent = recent.map((event) => `${event.kind} · ${event.status}`).join(" / "); row.append(history);
    }
    list.append(row);
  }
  if (!goals.length) list.innerHTML = '<p class="hint">No goals for this directory.</p>';
}

function renderRequests() {
  const box = $("requests"); box.replaceChildren();
  for (const item of state.approvals) {
    const card = cardFor("Tool permission", `${item.name || item.Name || "Tool"}: ${item.reason || item.Reason || "Permission requested"}`);
    addChoice(card, "Allow once", () => request("approval_resolve", { session_id: state.session, approval_id: item.id || item.ID, approval_choice: "allow_once" }));
    addChoice(card, "Save rule", () => request("approval_resolve", { session_id: state.session, approval_id: item.id || item.ID, approval_choice: "save_rule" }));
    addChoice(card, "Deny", () => request("approval_resolve", { session_id: state.session, approval_id: item.id || item.ID, approval_choice: "deny" }), true); box.append(card);
  }
  for (const item of state.questions) {
    const card = cardFor("Question", item.question || item.prompt || "Stable needs an answer.");
    const input = document.createElement("input"); input.placeholder = "Your answer"; card.append(input);
    addChoice(card, "Reply", () => request("reply", { session_id: state.session, question_id: item.id || item.question_id, text: input.value })); box.append(card);
  }
  for (const item of state.plans) {
    const card = cardFor("Plan approval", item.plan_path || "A plan is ready for review.");
    addChoice(card, "Approve", () => request("plan_resolve", { session_id: state.session, approval_choice: "auto" }));
    const feedback = document.createElement("input"); feedback.placeholder = "Feedback if changes are needed"; card.append(feedback);
    addChoice(card, "Request changes", () => request("plan_resolve", { session_id: state.session, approval_choice: "feedback", text: feedback.value }), true); box.append(card);
  }
}

function cardFor(title, text) { const card = document.createElement("section"); card.className = "request-card"; const h = document.createElement("h3"); h.textContent = title; const p = document.createElement("p"); p.textContent = text; card.append(h, p); return card; }
function addChoice(card, label, action, secondary = false) { const wrap = card.querySelector(".request-actions") || document.createElement("div"); wrap.className = "request-actions"; const button = document.createElement("button"); button.textContent = label; if (secondary) button.className = "deny"; button.addEventListener("click", action); wrap.append(button); if (!wrap.parentElement) card.append(wrap); }

window.addEventListener("beforeunload", () => { if (state.timer) clearInterval(state.timer); if (state.socket) state.socket.close(); });
