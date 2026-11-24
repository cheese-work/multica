// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  mergeFillGaps,
  mergeSnapshot,
  removeEntity,
  resolveEntities,
  upsertEntities,
  type EntityTable,
} from "./entity-table";

interface Row {
  id: string;
  a?: number;
  b?: number;
}

describe("entity table", () => {
  it("upserts a new id", () => {
    const table = upsertEntities<Row>(undefined, [{ id: "1", a: 1 }], mergeFillGaps);
    expect(table.byId).toEqual({ "1": { id: "1", a: 1 } });
  });

  it("mergeFillGaps: the existing row wins every field it defines", () => {
    expect(mergeFillGaps<Row>({ id: "1", a: 1 }, { id: "1", a: 9, b: 2 })).toEqual({
      id: "1",
      a: 1,
      b: 2,
    });
  });

  it("mergeSnapshot: incoming wins, fields only the existing row holds survive", () => {
    expect(mergeSnapshot<Row>({ id: "1", a: 1, b: 5 }, { id: "1", a: 9 })).toEqual({
      id: "1",
      a: 9,
      b: 5,
    });
  });

  it("keeps references when nothing changed", () => {
    const table: EntityTable<Row> = { byId: { "1": { id: "1", a: 1 } } };
    expect(upsertEntities(table, [{ id: "1", a: 1 }], mergeSnapshot)).toBe(table);
    expect(upsertEntities(table, [{ id: "1", b: undefined }], mergeFillGaps)).toBe(table);
    expect(removeEntity(table, "missing")).toBe(table);
  });

  it("resolves ordered ids, skipping unknown and repeated ids", () => {
    const table: EntityTable<Row> = { byId: { "1": { id: "1" }, "2": { id: "2" } } };
    expect(resolveEntities(table, ["2", "x", "1", "2"]).map((r) => r.id)).toEqual(["2", "1"]);
  });
});
