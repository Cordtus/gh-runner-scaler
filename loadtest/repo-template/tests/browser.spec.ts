import { expect, test } from "@playwright/test";

test("renders the load lab page", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator("#title")).toHaveText("Runner Load Lab");
});

test("renders many rows after interaction", async ({ page }) => {
  await page.goto("/");
  await page.locator("#render").click();
  await expect(page.locator("#rows li")).toHaveCount(240);
});
