import { renderFixture } from "./payload";

const rows = Array.from({ length: 32 }, (_, index) => renderFixture(0, index));

if (rows.length !== 32) {
  throw new Error("unexpected row count");
}

console.log(JSON.stringify(rows[0]));
