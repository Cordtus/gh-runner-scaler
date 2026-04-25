export type Fixture = {
  shard: number;
  item: number;
  checksum: string;
  tags: string[];
};

export function renderFixture(shard: number, item: number): Fixture {
  const checksum = `${shard}-${item}`.padEnd(24, "x");
  return {
    shard,
    item,
    checksum,
    tags: ["runner", "load", `shard-${shard}`]
  };
}
