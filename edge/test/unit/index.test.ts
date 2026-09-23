import { exports } from "cloudflare:workers";
import { describe, expect, it } from "vitest";

type Echo = { host: string | null; kind: string; name: string | null; baseDomain: string };

async function echo(host: string): Promise<Echo> {
  const res = await exports.default.fetch("https://placeholder/", { headers: { host } });
  expect(res.status).toBe(200);
  return res.json();
}

describe("skeleton worker", () => {
  it("classifies a tunnel host from the Host header", async () => {
    const body = await echo("shop.tuzy.dev");
    expect(body).toMatchObject({ host: "shop.tuzy.dev", kind: "tunnel", name: "shop" });
  });

  it("classifies the apex", async () => {
    expect(await echo("tuzy.dev")).toMatchObject({ kind: "apex", name: null });
  });

  it("classifies a foreign host", async () => {
    expect(await echo("example.com")).toMatchObject({ kind: "foreign", name: null });
  });
});
