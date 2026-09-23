import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { FrameType } from "../../src/protocol/frames";
import { api, lastCode, login } from "./api-helpers";
import { FakeAgent } from "./fake-agent";

let n = 0;
const email = () => `tok${n++}.${Date.now()}@example.com`;

describe("token lifecycle", () => {
  it("creates a connect-scope CI token that can connect but not manage", async () => {
    const { token } = await login(email());
    const created = await api("/tokens", { token, body: { label: "ci", expires_in_days: 30 } });
    expect(created.status).toBe(201);
    expect(created.json).toMatchObject({ scope: "connect", label: "ci" });
    const ci = created.json.token as string;
    expect((await api("/me", { token: ci })).status).toBe(200);
    const mutate = await api("/tokens", { token: ci, body: { label: "x" } });
    expect(mutate.status).toBe(403);
    expect(mutate.json.error.code).toBe("insufficient_scope");
    const agent = await FakeAgent.connect("ci-scope", { token: ci });
    agent.close();
  });

  it("lists tokens and marks the current one", async () => {
    const { token } = await login(email());
    await api("/tokens", { token, body: { label: "ci" } });
    const list = await api("/tokens", { token });
    expect(list.json.tokens).toHaveLength(2);
    expect(list.json.tokens.filter((t: { current: boolean }) => t.current)).toHaveLength(1);
  });

  it("revoking a token 401s it and closes its live tunnels (GOAWAY revoked)", async () => {
    const { token } = await login(email());
    const agent = await FakeAgent.connect("revoke-live", { token });
    const del = await api("/tokens/current", { method: "DELETE", token });
    expect(del.status).toBe(200);
    expect((await api("/me", { token })).status).toBe(401);
    expect((await agent.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "revoked" });
    await agent.waitClosed();
    const { res } = await FakeAgent.open("revoke-live", { token });
    expect(res.status).toBe(401);
  });

  it("revoking one device closes only that device's tunnel (two devices, one name each)", async () => {
    const e = email();
    const a = await login(e, "laptop");
    const b = await login(e, "desktop");
    const agentA = await FakeAgent.connect("dev-a-name", { token: a.token });
    const agentB = await FakeAgent.connect("dev-b-name", { token: b.token });
    // Device B also tries A's name: 409, and must not steal the session record.
    const { res } = await FakeAgent.open("dev-a-name", { token: b.token });
    expect(res.status).toBe(409);
    await new Promise((r) => setTimeout(r, 100));
    expect((await api("/tokens/current", { method: "DELETE", token: a.token })).status).toBe(200);
    expect((await agentA.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "revoked" });
    await new Promise((r) => setTimeout(r, 200));
    expect(agentB.closed).toBeNull();
    agentB.close();
  });

  it("revokes another of your own tokens by id and closes its tunnel", async () => {
    const e = email();
    const main = await login(e);
    const ci = (await api("/tokens", { token: main.token, body: { label: "ci" } })).json;
    const agent = await FakeAgent.connect("ci-owned", { token: ci.token });
    expect((await api(`/tokens/${ci.id}`, { method: "DELETE", token: main.token })).status).toBe(200);
    expect((await agent.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "revoked" });
  });

  it("connect-scope tokens are refused on every management route", async () => {
    const { token } = await login(email());
    const ci = (await api("/tokens", { token, body: {} })).json.token as string;
    for (const [path, method] of [
      ["/tokens", "GET"],
      ["/tokens/current", "DELETE"],
      ["/account/delete/start", "POST"],
    ] as const) {
      const r = await api(path, { method, token: ci, body: method === "POST" ? { name: "x-y-z" } : undefined });
      expect(r.json?.error?.code, `${method} ${path}`).toBe("insufficient_scope");
    }
  });

  it("cannot revoke someone else's token", async () => {
    const a = await login(email());
    const b = await login(email());
    const bTokens = await api("/tokens", { token: b.token });
    const del = await api(`/tokens/${bTokens.json.tokens[0].id}`, { method: "DELETE", token: a.token });
    expect(del.status).toBe(404);
    expect((await api("/me", { token: b.token })).status).toBe(200);
  });

  it("rejects expired and idle (> 90 d) tokens with token_expired", async () => {
    const { token, userId } = await login(email());
    const old = Math.floor(Date.now() / 1000) - 91 * 86400;
    await env.DB.prepare("UPDATE tokens SET last_used_at = ?1, created_at = ?1 WHERE user_id = ?2").bind(old, userId).run();
    const idle = await api("/me", { token });
    expect(idle.status).toBe(401);
    expect(idle.json.error.code).toBe("token_expired");

    const fresh = await login(email());
    const ci = (await api("/tokens", { token: fresh.token, body: { expires_in_days: 1 } })).json;
    await env.DB.prepare("UPDATE tokens SET expires_at = 1 WHERE id = ?1").bind(ci.id).run();
    expect((await api("/me", { token: ci.token })).json.error.code).toBe("token_expired");
  });

  it("suspended users are refused", async () => {
    const e = email();
    const { token, userId } = await login(e);
    await env.DB.prepare("UPDATE users SET status = 'suspended' WHERE id = ?1").bind(userId).run();
    expect((await api("/me", { token })).json.error.code).toBe("account_suspended");
    const start = await api("/auth/email/start", { body: { email: e } });
    const v = await api("/auth/email/verify", { body: { login_id: start.json.login_id, code: lastCode(e) } });
    expect(v.status).toBe(403);
    expect(v.json.error.code).toBe("account_suspended");
  });

  it("writes last_used_at at most hourly (no D1 write on the hot path)", async () => {
    const { token, userId } = await login(email());
    await api("/me", { token });
    await new Promise((r) => setTimeout(r, 50));
    const t1 = await env.DB.prepare("SELECT last_used_at FROM tokens WHERE user_id = ?1").bind(userId).first<{ last_used_at: number }>();
    await env.DB.prepare("UPDATE tokens SET last_used_at = last_used_at - 60 WHERE user_id = ?1").bind(userId).run();
    await api("/me", { token });
    await new Promise((r) => setTimeout(r, 50));
    const t2 = await env.DB.prepare("SELECT last_used_at FROM tokens WHERE user_id = ?1").bind(userId).first<{ last_used_at: number }>();
    expect(t2!.last_used_at).toBe(t1!.last_used_at - 60); // within the hour: untouched
  });
});

describe("account deletion", () => {
  it("deletes with an emailed code: tokens die and live tunnels get GOAWAY deleted", async () => {
    const e = email();
    const { token, userId } = await login(e);
    const agent = await FakeAgent.connect("delete-me", { token });
    const start = await api("/account/delete/start", { token, body: {} });
    expect(start.status).toBe(200);
    const confirm = await api("/account/delete/confirm", { token, body: { login_id: start.json.login_id, code: lastCode(e) } });
    expect(confirm.status, JSON.stringify(confirm.json)).toBe(200);
    expect((await agent.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "deleted" });
    expect((await api("/me", { token })).status).toBe(401);
    const user = await env.DB.prepare("SELECT status, deleted_at FROM users WHERE id = ?1").bind(userId).first<{ status: string; deleted_at: number }>();
    expect(user).toMatchObject({ status: "deleted" });
    const relogin = await api("/auth/email/start", { body: { email: e } });
    const v = await api("/auth/email/verify", { body: { login_id: relogin.json.login_id, code: lastCode(e) } });
    expect(v.json.error.code).toBe("account_deleted");
  });

  it("deleting an account only closes that account's sockets", async () => {
    const victim = await login(email());
    const attacker = await login(email());
    const agent = await FakeAgent.connect("victim-live", { token: victim.token });
    const { res } = await FakeAgent.open("victim-live", { token: attacker.token }); // 409: not recorded
    expect(res.status).toBe(409);
    const e = (await api("/me", { token: attacker.token })).json.user.email as string;
    const start = await api("/account/delete/start", { token: attacker.token, body: {} });
    expect((await api("/account/delete/confirm", { token: attacker.token, body: { login_id: start.json.login_id, code: lastCode(e) } })).status).toBe(200);
    await new Promise((r) => setTimeout(r, 300));
    expect(agent.closed).toBeNull();
    agent.close();
  });

  it("a login code cannot confirm a deletion", async () => {
    const e = email();
    const { token } = await login(e);
    const loginStart = await api("/auth/email/start", { body: { email: e } });
    const r = await api("/account/delete/confirm", { token, body: { login_id: loginStart.json.login_id, code: lastCode(e) } });
    expect(r.status).toBe(400);
    expect((await api("/me", { token })).status).toBe(200);
  });
});
