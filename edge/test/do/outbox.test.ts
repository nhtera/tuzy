import { env } from "cloudflare:workers";
import { createScheduledController } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { outboxRow, pushOutbox } from "../../src/lib/outbox";
import { scheduled } from "../../src/scheduled";
import { FrameType } from "../../src/protocol/frames";
import { FakeAgent } from "./fake-agent";

describe("D1 → DO outbox", () => {
  it("pushes a row to the DO and marks it done", async () => {
    const agent = await FakeAgent.connect("obx-goaway");
    const { id, stmt } = outboxRow(env.DB, "obx-goaway", { action: "goaway", payload: { reason: "renamed", newName: "obx-new" } });
    await stmt.run();
    expect(await pushOutbox(env, [id])).toEqual({ done: 1, failed: 0 });
    expect((await agent.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "renamed", new_name: "obx-new" });
    const row = await env.DB.prepare("SELECT done_at FROM do_outbox WHERE id = ?1").bind(id).first<{ done_at: number }>();
    expect(row!.done_at).toBeGreaterThan(0);
  });

  it("backs off failed rows and the cron retries due rows", async () => {
    await env.DB.prepare("INSERT INTO do_outbox (id, name, action, payload, next_at, created_at) VALUES ('obx_bad', 'x-name', 'nope', '{}', 0, 0)").run();
    expect((await pushOutbox(env, ["obx_bad"])).failed).toBe(1);
    const bad = await env.DB.prepare("SELECT attempts, next_at FROM do_outbox WHERE id = 'obx_bad'").first<{ attempts: number; next_at: number }>();
    expect(bad!.attempts).toBe(1);
    expect(bad!.next_at).toBeGreaterThan(Math.floor(Date.now() / 1000));

    const agent = await FakeAgent.connect("obx-cron");
    await outboxRow(env.DB, "obx-cron", { action: "revokeToken", payload: { tokenId: "tok_test000000000000" } }).stmt.run();
    const ctrl = createScheduledController({ cron: "*/5 * * * *", scheduledTime: Date.now() });
    await scheduled(ctrl, env);
    expect((await agent.next((f) => f.type === FrameType.GOAWAY)).json).toMatchObject({ reason: "revoked" });
  });
});
