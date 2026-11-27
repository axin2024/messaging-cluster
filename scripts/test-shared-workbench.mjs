import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.MESSAGING_PLAYWRIGHT_MODULE || "playwright");
const chunks = [];
for await (const chunk of process.stdin) chunks.push(chunk);
const fixture = JSON.parse(Buffer.concat(chunks).toString());

const browser = await chromium.launch({ executablePath: process.env.MESSAGING_BROWSER_BIN || undefined });
const message = "procurement browser evidence";
const filename = "spec-browser.txt";

function responseFor(page, method, path) {
  return page.waitForResponse((response) => {
    const pathname = new URL(response.url()).pathname;
    return response.request().method() === method && (typeof path === "string" ? pathname === path : path.test(pathname));
  });
}

async function interact(page, method, path, status, action) {
  const [response] = await Promise.all([responseFor(page, method, path), action()]);
  assert.equal(response.status(), status, `${method} ${new URL(response.url()).pathname}`);
  return response;
}

async function workbench(origin) {
  const page = await browser.newPage({ acceptDownloads: true });
  page.setDefaultTimeout(8000);
  await page.goto(`${origin}/demo/project.html`);
  return page;
}

async function fields(page, form, values) {
  for (const [name, value] of Object.entries(values)) await page.locator(`${form} [name=${name}]`).fill(String(value));
}

async function adminPage(origin, token) {
  const page = await workbench(origin);
  await page.locator("#admin-mode").click();
  await fields(page, "#admin-login", { token });
  await page.locator("#admin-login button").click();
  await page.locator("#admin-workspace").waitFor();
  return page;
}

async function grant(page, linkID, user) {
  await fields(page, "#change-grant", { user, version: 0 });
  const response = await interact(page, "POST", `/v1/admin/partner-links/${linkID}/grants`, 200,
    () => page.locator("#change-grant button[value=grant]").click());
  await page.locator("#grant-proof").getByText(/已授权/).waitFor();
  return response.json();
}

async function memberPage(origin, tenant, username, deviceKey) {
  const page = await workbench(origin);
  await fields(page, "#member-login", { tenant, username, device_key: deviceKey, password: fixture.password });
  await page.locator("#member-login button").click();
  await page.locator("#member-links button").click();
  await page.locator("#member-channels button").click();
  await page.locator("#timeline li").first().waitFor();
  return page;
}

try {
  const [host, peer] = await Promise.all([
    adminPage(fixture.host_origin, fixture.host_admin),
    adminPage(fixture.peer_origin, fixture.peer_admin),
  ]);
  await host.locator("summary").getByText("创建空间", { exact: true }).click();
  await fields(host, "#create-space", { key: "browser-procurement", title: "跨组织采购确认", purpose: "验证采购文件的授权与撤回" });
  const created = await interact(host, "POST", "/v1/admin/shared-spaces", 201, () => host.locator("#create-space button").click());
  const { space_id: spaceID } = await created.json();
  await host.locator("summary").getByText("邀请合作组织", { exact: true }).click();
  await fields(host, "#invite", { peer_tenant: fixture.peer_tenant });
  await host.locator(`#invite-spaces option[value="${spaceID}"]`).waitFor({ state: "attached" });
  await interact(host, "POST", `/v1/admin/shared-spaces/${spaceID}/invitations`, 201, () => host.locator("#invite button").click());
  await peer.locator("#admin-refresh").click();
  const signed = await interact(peer, "POST", new RegExp(`^/shared-spaces/${spaceID}/partners/\\d+/accept$`), 200,
    () => peer.locator("#admin-invitations").getByRole("button", { name: "签认邀请" }).click());
  const { link_id: linkID } = await signed.json();
  await host.locator("#admin-refresh").click();
  await host.locator("#admin-links button").click();
  await host.locator("#channel-controls summary").click();
  await fields(host, "#create-channel", { key: "dispatch", title: "到货规格" });
  const channel = await interact(host, "POST", `/v1/admin/partner-links/${linkID}/channels`, 201,
    () => host.locator("#create-channel button").click());
  const { channel_id: channelID } = await channel.json();
  await grant(host, linkID, fixture.host_user);
  const supplierGrant = await grant(peer, linkID, fixture.peer_user);

  const buyer = await memberPage(fixture.host_origin, fixture.host_tenant, "buyer", "buyer-web");
  await buyer.locator("#message-body").fill(message);
  await buyer.locator("#compose input[type=file]").setInputFiles({ name: filename, mimeType: "text/plain", buffer: Buffer.from("Synthetic purchase specification.\n") });
  const sent = await interact(buyer, "POST", `/v1/shared-channels/${channelID}/messages`, 201,
    () => buyer.locator("#compose button[type=submit]").click());
  const { seq } = await sent.json();
  await buyer.locator("#timeline").getByText(message, { exact: true }).waitFor();
  const preview = await interact(host, "GET", `/v1/admin/partner-links/${linkID}/export-preview`, 200,
    () => host.locator("#preview-export").click());
  assert.deepEqual(await preview.json(), { snapshot_project_seq: 1, item_count: 1 });
  await host.locator("#export-preview").getByText(/有 1 条可归档消息/).waitFor();

  const [file, search, reconnect] = await Promise.all(["supplier-file", "supplier-search", "supplier-offline"].map(
    (device) => memberPage(fixture.peer_origin, fixture.peer_tenant, "supplier", device),
  ));
  for (const device of [file, search, reconnect]) await device.locator("#timeline").getByText(message, { exact: true }).waitFor();
  await reconnect.context().setOffline(true);
  await fields(search, "#search", { q: "procurement" });
  const searchPath = `/v1/partner-links/${linkID}/search`;
  await interact(search, "GET", searchPath, 200, () => search.locator("#search button").click());
  await search.locator("#search-list").getByText(message, { exact: true }).waitFor();

  // Hold the browser's download request so the ticket remains unused until after the other gateway commits the revoke.
  const contentPath = "/v1/shared-attachment-content";
  const held = Promise.withResolvers();
  const release = Promise.withResolvers();
  let attachmentSent;
  let downloads = 0;
  file.on("download", () => { downloads++; });
  await file.route(`**${contentPath}`, async (route) => {
    held.resolve();
    await release.promise;
    attachmentSent = Date.now();
    await route.continue();
  });
  await interact(file, "POST", new RegExp(`^/v1/shared-channels/${channelID}/messages/${seq}/attachments/\\d+/grant$`), 201,
    () => file.getByRole("button", { name: `下载 ${filename}` }).click());
  const holdTimeout = setTimeout(() => held.reject(new Error("browser did not start the attachment content request")), 8000);
  try {
    await held.promise;
  } finally {
    clearTimeout(holdTimeout);
  }

  await fields(host, "#change-grant", { user: fixture.peer_user, version: supplierGrant.grant_version });
  const revoke = await interact(host, "POST", `/v1/admin/partner-links/${linkID}/grants`, 200,
    () => host.locator("#change-grant button[value=revoke]").click());
  const revokeConfirmed = Date.now();
  const receipt = await revoke.json();
  assert.equal(receipt.revoked, true);
  assert.equal(receipt.device_authorizations.length, 3);
  assert.equal(new Set(receipt.device_authorizations.map((device) => device.device_id)).size, 3);
  for (const device of receipt.device_authorizations) assert.equal(device.grant_epoch + 1, receipt.grant_epoch);
  await host.locator("#grant-proof").getByText(/已撤权/).waitFor();
  assert.equal(await host.locator("#grant-proof").getByText(/上次成功鉴权代际/).count(), 3);

  const fileDenied = await interact(file, "GET", contentPath, 404, () => release.resolve());
  assert.deepEqual(await fileDenied.json(), { error: "not_found" });
  await file.locator("#timeline li").first().waitFor({ state: "detached" });
  assert.equal(downloads, 0);

  const searchSent = Date.now();
  const searchDenied = await interact(search, "GET", searchPath, 404, () => search.locator("#search button").click());
  assert.deepEqual(await searchDenied.json(), { error: "not_found" });
  await search.locator("#search-results").waitFor({ state: "hidden" });
  await search.locator("#timeline li").first().waitFor({ state: "detached" });

  await reconnect.context().setOffline(false);
  const reconnectSent = Date.now();
  const refreshed = await interact(reconnect, "GET", "/v1/shared-projects", 200, () => reconnect.locator("#member-refresh").click());
  assert.deepEqual((await refreshed.json()).items, []);
  await reconnect.locator("#feedback").getByText(/此前打开的项目已不可访问/).waitFor();
  assert.equal(await reconnect.locator("#timeline li").count(), 0);

  console.log(JSON.stringify({
    link_id: linkID, channel_id: channelID, seq, grant_epoch: receipt.grant_epoch,
    revoke_confirmed_ms: revokeConfirmed, attachment_sent_ms: attachmentSent,
    search_sent_ms: searchSent, reconnect_sent_ms: reconnectSent, device_authorizations: receipt.device_authorizations.length,
  }));
} finally {
  await browser.close();
}
