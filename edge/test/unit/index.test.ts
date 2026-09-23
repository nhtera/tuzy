import { exports } from "cloudflare:workers";
import { describe, expect, it } from "vitest";

async function get(host: string): Promise<{ status: number; body: string }> {
  const res = await exports.default.fetch("https://placeholder/", { headers: { host } });
  return { status: res.status, body: await res.text() };
}

describe("skeleton worker", () => {
  it("classifies a tunnel host from the Host header", async () => {
    expect(await get("shop.tuzy.dev")).toEqual({ status: 200, body: "tuzy: tunnel shop\n" });
  });

  it("classifies the apex", async () => {
    expect(await get("tuzy.dev")).toEqual({ status: 200, body: "tuzy: apex\n" });
  });

  it("rejects a foreign host with 404", async () => {
    expect(await get("example.com")).toEqual({ status: 404, body: "tuzy: unknown host\n" });
  });
});
