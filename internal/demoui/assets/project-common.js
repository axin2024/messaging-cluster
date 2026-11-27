export const $ = (id) => document.getElementById(id);

export function element(tag, value, className) {
  const node = document.createElement(tag);
  node.textContent = value;
  if (className) node.className = className;
  return node;
}

export function feedback(message = "", error = false) {
  $("feedback").textContent = message;
  $("feedback").dataset.error = String(error);
}

export async function request(token, path, options = {}) {
  const headers = new Headers(options.headers);
  if (token) headers.set("Authorization", `Bearer ${token}`);
  const response = await fetch(path, { ...options, headers, cache: "no-store" });
  if (!response.ok) {
    const problem = await response.json().catch(() => ({}));
    const codes = {
      refresh_shared_cursor: "授权版本已变化，请重新读取频道或搜索。",
      invitation_expired: "邀请已过签认截止时间。",
      unauthorized: "会话或管理凭证已失效。",
      not_found: "项目权限已变化，或此内容不可访问。",
      forbidden: "当前授权不允许执行此操作。",
      already_exists: "当前状态或版本不允许此变更，请刷新后核对。",
    };
    const error = new Error(codes[problem.error] || `请求失败（${response.status}${problem.error ? ` · ${problem.error}` : ""}）`);
    error.status = response.status;
    error.code = problem.error;
    throw error;
  }
  return response;
}

export async function json(token, path, method = "GET", body, headers = {}) {
  if (body !== undefined) headers = { ...headers, "Content-Type": "application/json" };
  const response = await request(token, path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  return response.status === 204 ? undefined : response.json();
}

export function key() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export function identifier(value, allowZero = false) {
  const id = Number(value);
  if (!Number.isSafeInteger(id) || id < (allowZero ? 0 : 1)) throw new Error("请输入有效的编号或版本。");
  return id;
}

export function absoluteDate(value) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) throw new Error("请填写有效的截止时间。");
  return date.toISOString();
}

export function localDate(value) {
  const date = new Date(value);
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}

export function displayDate(value) {
  return new Date(value).toLocaleString("zh-CN", { hour12: false });
}

function asyncAction(control, operation) {
  return async (...args) => {
    control.disabled = true;
    feedback();
    try {
      await operation(...args);
    } catch (error) {
      feedback(error.message, true);
    } finally {
      control.disabled = false;
    }
  };
}

export function action(control, operation) {
  control.addEventListener("click", asyncAction(control, operation));
}

export function form(id, handler) {
  const node = $(id);
  node.addEventListener("submit", (event) => {
    event.preventDefault();
    const submitter = event.submitter || node.querySelector('button[type="submit"]');
    const data = new FormData(node);
    return asyncAction(submitter, handler)(data, submitter.value);
  });
}

export function rowButton(label, current, handler) {
  const button = element("button", label);
  button.type = "button";
  if (current !== undefined) button.setAttribute("aria-current", String(current));
  action(button, handler);
  return button;
}

export function list(node, items, empty) {
  node.replaceChildren(...(items.length ? items : [element("p", empty, "muted")]));
}

export async function saveResponse(response, filename) {
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const anchor = element("a", "");
  anchor.href = url;
  anchor.download = filename;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
