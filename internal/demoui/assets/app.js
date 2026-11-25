"use strict";

const byID = (id) => document.getElementById(id);
const login = byID("login-form");
const composer = byID("composer");
const connection = byID("connection");
let current;

function element(tag, value, className) {
  const node = document.createElement(tag);
  node.textContent = value;
  if (className) node.className = className;
  return node;
}

async function request(session, path, options = {}) {
  const headers = new Headers(options.headers);
  if (session) headers.set("Authorization", `Bearer ${session.token}`);
  const response = await fetch(path, { ...options, headers, cache: "no-store" });
  if (!response.ok) {
    const problem = await response.json().catch(() => ({}));
    throw new Error(`请求失败（${response.status}${problem.error ? ` · ${problem.error}` : ""}）`);
  }
  return response;
}

async function json(session, path, method = "GET", data) {
  const options = { method };
  if (data !== undefined) {
    options.headers = { "Content-Type": "application/json" };
    options.body = JSON.stringify(data);
  }
  return (await request(session, path, options)).json();
}

function feedback(message = "") {
  byID("feedback").textContent = message;
}

function bindForm(form, handler) {
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector('button[type="submit"]');
    button.disabled = true;
    feedback();
    try {
      await handler(new FormData(form));
    } catch (error) {
      feedback(error.message);
    } finally {
      button.disabled = false;
    }
  });
}

function action(node, handler) {
  node.addEventListener("click", async () => {
    node.disabled = true;
    feedback();
    try {
      await handler();
    } catch (error) {
      feedback(error.message);
    } finally {
      node.disabled = false;
    }
  });
}

function idFrom(value) {
  const id = Number(value);
  if (!Number.isSafeInteger(id) || id <= 0) throw new Error("请输入有效的用户 ID。");
  return id;
}

function base(id) {
  return `/v1/conversations/${id}`;
}

function title(room, user) {
  if (room.kind === "group") return room.title;
  const peer = room.direct_low === user ? room.direct_high : room.direct_low;
  return `与用户 ${peer} 的私聊`;
}

async function collection(session, path, gaps) {
  const items = [];
  let after = 0;
  do {
    const page = await json(session, `${path}?after=${after}`);
    items.push(...(page.items || []));
    if (gaps) gaps.push(...(page.expired || []));
    const next = page.has_more === false ? 0 : (page.next_after || 0);
    if (next && next <= after) throw new Error("同步游标没有前进。");
    after = next;
  } while (after);
  return items;
}

async function update(session) {
  const [rooms, inbox] = await Promise.all([collection(session, "/v1/conversations"), collection(session, "/v1/inbox")]);
  if (current !== session) return;
  const summaries = new Map(inbox.map((item) => [item.conversation_id, item]));
  const selected = session.selected;
  const room = rooms.find((item) => item.id === selected);
  byID("rooms").replaceChildren(...rooms.map((item) => {
    const li = document.createElement("li");
    const button = element("button", title(item, session.profile.user_id));
    button.type = "button";
    button.setAttribute("aria-current", String(item.id === selected));
    const summary = summaries.get(item.id);
    if (summary) button.append(element("small", `最新序号 ${summary.high_seq} · 已读 ${summary.read_seq}`));
    action(button, () => selectRoom(session, item.id));
    li.append(button);
    return li;
  }));
  byID("empty").hidden = Boolean(room);
  byID("opened").hidden = !room;
  if (!room) return;

  const path = base(selected);
  const gaps = [];
  const [messages, read] = await Promise.all([collection(session, `${path}/sync`, gaps), json(session, `${path}/read`)]);
  if (current !== session || session.selected !== selected) return;
  const entry = summaries.get(selected);
  const high = messages.at(-1)?.seq || 0;
  session.high = high;
  let sync = entry?.sync_seq || 0;
  if (high > sync) sync = (await json(session, `${path}/sync`, "PUT", { seq: high })).sync_seq;
  if (current !== session || session.selected !== selected) return;
  byID("room-name").textContent = title(room, session.profile.user_id);
  const gapLabel = gaps.length ? ` · 历史已过期 ${gaps.map((gap) => `${gap.from}–${gap.to}`).join("、")}` : "";
  byID("room-state").textContent = `会话 ${selected} · 设备游标 ${sync} · 已读 ${read.read_seq} · 未读 ${read.unread}${gapLabel}`;
  byID("add-member").hidden = room.kind !== "group";
  byID("mark-read").disabled = !high || high <= read.read_seq;
  renderMessages(session, messages);
}

function refresh(session) {
  session.dirty = true;
  if (!session.refreshing) {
    session.refreshing = (async () => {
      while (current === session && session.dirty) {
        session.dirty = false;
        try {
          await update(session);
        } catch (error) {
          if (current === session) feedback(error.message);
        }
      }
    })().finally(() => { session.refreshing = null; });
  }
  return session.refreshing;
}

function selectRoom(session, id) {
  if (session.selected !== id) {
    session.selected = id;
    session.high = 0;
    session.draft = null;
    byID("send").textContent = "发送";
    composer.reset();
    byID("messages").replaceChildren();
    byID("room-name").textContent = "正在同步";
    byID("room-state").textContent = "";
    byID("opened").hidden = true;
  }
  return refresh(session);
}

function renderMessages(session, messages) {
  const list = byID("messages");
  const atEnd = list.scrollHeight - list.clientHeight - list.scrollTop < 36;
  list.replaceChildren(...messages.map((message) => {
    const self = message.sender_id === session.profile.user_id;
    const item = element("li", "", self ? "self" : "");
    item.append(element("div", `#${message.seq} · ${self ? "我" : `用户 ${message.sender_id}`}`, "message-meta"));
    item.append(element("div", message.retracted_at ? "消息已撤回" : message.expired ? "消息已过期" : message.body, "message-body"));
    const actions = element("div", "", "message-actions");
    for (const id of message.attachment_ids || []) {
      const button = element("button", `下载附件 ${id}`);
      action(button, () => download(session, message, id));
      actions.append(button);
    }
    if (self && !message.retracted_at && !message.expired) {
      const button = element("button", "撤回");
      action(button, async () => {
        await json(session, `${base(message.conversation_id)}/messages/${message.seq}`, "DELETE");
        await refresh(session);
      });
      actions.append(button);
    }
    if (actions.childElementCount) item.append(actions);
    return item;
  }));
  if (atEnd) list.scrollTop = list.scrollHeight;
}

async function download(session, message, attachment) {
  const grant = await json(session, `${base(message.conversation_id)}/messages/${message.seq}/attachments/${attachment}/grants`, "POST");
  const response = await request(null, "/v1/attachment-content", { headers: { Authorization: `Attachment ${grant.token}` } });
  const url = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = `attachment-${attachment}`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

async function connect(session) {
  if (connection.dataset.state === "connecting" || session.socket?.readyState === WebSocket.OPEN) return;
  connection.dataset.state = "connecting";
  connection.textContent = "正在接入";
  try {
    const { ticket } = await json(session, "/v1/gateway/tickets", "POST");
    if (current !== session) return;
    const url = new URL("/v1/gateway", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(url, ["messaging.v1", `ticket.${ticket}`]);
    session.socket = socket;
    socket.addEventListener("message", ({ data }) => {
      if (current !== session) return;
      const frame = JSON.parse(data);
      if (frame.type === "ready") {
        connection.dataset.state = "on";
        connection.textContent = `${frame.node.name} · 连接代数 ${frame.connection_epoch}`;
        refresh(session);
      } else if (frame.type === "notification.resync_required") {
        feedback("正在补拉消息。刷新可重建实时连接。");
        socket.close();
        refresh(session);
      } else if (frame.type.startsWith("notification.")) {
        socket.send(JSON.stringify({ type: "notification.ack", delivery_id: frame.delivery_id }));
        refresh(session);
      }
    });
    socket.addEventListener("close", () => {
      if (current !== session || socket !== session.socket) return;
      connection.dataset.state = "off";
      connection.textContent = "连接已断开";
    });
  } catch (error) {
    if (current === session) {
      connection.dataset.state = "off";
      connection.textContent = "连接未建立";
    }
    throw error;
  }
}

bindForm(login, async (data) => {
  const credentials = Object.fromEntries(data);
  const { token, session: profile } = await json(null, "/v1/sessions", "POST", credentials);
  const session = { token, profile, selected: 0, high: 0 };
  current = session;
  login.elements.password.value = "";
  byID("signin").hidden = true;
  byID("workspace").hidden = false;
  byID("logout").hidden = false;
  byID("accounts").textContent = `${profile.display_name} · 用户 ${profile.user_id} · 设备 ${profile.device_key}`;
  await refresh(session);
  await connect(session);
});

action(byID("logout"), async () => {
  const session = current;
  current = undefined;
  session.socket?.close();
  byID("signin").hidden = false;
  byID("workspace").hidden = true;
  byID("logout").hidden = true;
  byID("messages").replaceChildren();
  byID("rooms").replaceChildren();
  byID("opened").hidden = true;
  byID("empty").hidden = false;
  byID("send").textContent = "发送";
  composer.reset();
  connection.dataset.state = "off";
  connection.textContent = "未连接";
  login.elements.username.focus();
  await request(session, `/v1/devices/${session.profile.device_id}`, { method: "DELETE" });
});

action(byID("reload"), async () => {
  const session = current;
  await refresh(session);
  if (current === session) await connect(session);
});

const direct = byID("create-direct");
bindForm(direct, async (data) => {
  const session = current;
  const room = await json(session, "/v1/conversations/direct", "POST", { user_id: idFrom(data.get("user_id")) });
  if (current !== session) return;
  direct.reset();
  await selectRoom(session, room.id);
});

const group = byID("create-group");
bindForm(group, async (data) => {
  const session = current;
  const room = await json(session, "/v1/conversations/group", "POST", { title: data.get("title").trim(), history_policy: "since_join" });
  if (current !== session) return;
  group.reset();
  await selectRoom(session, room.id);
});

const member = byID("add-member");
bindForm(member, async (data) => {
  const session = current;
  await json(session, `${base(session.selected)}/members/${idFrom(data.get("user_id"))}`, "PUT", { role: "member" });
  if (current === session) member.reset();
});

action(byID("mark-read"), async () => {
  const session = current;
  if (!session.high) return;
  await json(session, `${base(session.selected)}/read`, "PUT", { seq: session.high });
  await refresh(session);
});

async function upload(session, file) {
  if (!file.size || file.size > 10 * 1024 * 1024) throw new Error("附件必须介于 1 字节和 10 MiB 之间。");
  const payload = await file.arrayBuffer();
  const hash = await crypto.subtle.digest("SHA-256", payload);
  const sha256 = [...new Uint8Array(hash)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
  const attachment = await json(session, "/v1/attachments", "POST", { filename: file.name, content_type: "application/octet-stream", size: file.size, sha256 });
  await request(session, `/v1/attachments/${attachment.id}/content`, { method: "PUT", body: payload });
  return attachment.id;
}

bindForm(composer, async (data) => {
  const session = current;
  const conversation = session.selected;
  const body = data.get("body");
  const file = data.get("file");
  if (!body.trim() && !file.name) throw new Error("请输入消息或选择附件。");
  if (!session.draft) session.draft = { client_id: crypto.randomUUID(), body };
  const draft = session.draft;
  if (file.name && !draft.attachment_ids) draft.attachment_ids = [await upload(session, file)];
  if (current !== session || session.selected !== conversation || session.draft !== draft) return;
  byID("send").textContent = "重试发送";
  await json(session, `${base(conversation)}/messages`, "POST", draft);
  if (current !== session || session.selected !== conversation || session.draft !== draft) return;
  session.draft = null;
  composer.reset();
  byID("send").textContent = "发送";
  feedback("已落库");
  await refresh(session);
});

composer.addEventListener("input", () => {
  if (current) current.draft = null;
  byID("send").textContent = "发送";
});
