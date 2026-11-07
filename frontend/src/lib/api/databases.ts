import { json, resource, resourceID, type ApiClient } from "./resources";

export type DatabaseChoice = {
  id: string;
  name: string;
  access_level: 1 | 2;
  capabilities: { create_migration: boolean };
};

export type Connection = {
  id: string;
  name: string;
  host: string;
  port: number;
  database: string;
  username: string;
  sslmode: "require" | "disable" | "verify-full";
};

export type ConnectionInput = Omit<Connection, "id"> & { password?: string };
type DatabaseDTO = { id: string; name: string; level: 1 | 2 };
type ConnectionDTO = Omit<Connection, "sslmode"> & { ssl_mode: Connection["sslmode"] };

function databaseChoice(item: DatabaseDTO): DatabaseChoice {
  return {
    id: item.id,
    name: item.name,
    access_level: item.level,
    capabilities: { create_migration: item.level === 2 },
  };
}

function connectionInput({ sslmode, ...input }: ConnectionInput) {
  return { ...input, ssl_mode: sslmode };
}
export type Table = { name: string; kind: string };
export type Definition = { name: string; sql: string };
export type TableDetails = {
  schema: string;
  name: string;
  columns: { name: string; type: string; nullable: boolean; default: string }[];
  constraints: Definition[];
  indexes: Definition[];
};
export type DataPage = {
  columns: string[];
  rows: { text: string; null: boolean; truncated: boolean }[][];
  page: number;
  has_next: boolean;
};
export type SchemaDiff = {
  reference_schema: string;
  target_schema: string;
  changes: {
    kind: string;
    action: string;
    table: string;
    name: string;
    before: string;
    after: string;
    risk: "review" | "destructive" | "";
    sql: string;
  }[];
  unsupported: { schema: string; table: string; name: string; kind: string }[];
  sql: string;
};

export function databaseApi(api: ApiClient) {
  const path = (id: string) => `/databases/${resourceID(id)}`;
  const connection = (id: string) => `/connections/${resourceID(id)}`;
  const query = (values: Record<string, string | number>) =>
    new URLSearchParams(Object.entries(values).map(([key, value]) => [key, String(value)]));
  return {
    databases: async (signal?: AbortSignal) => {
      const data = await resource<{ databases: DatabaseDTO[] }>(api, "/databases", signal);
      return { databases: data.databases.map(databaseChoice) };
    },
    database: async (id: string, signal?: AbortSignal) => {
      const data = await resource<{ database: DatabaseDTO }>(api, path(id), signal);
      return databaseChoice(data.database);
    },
    connections: async (signal?: AbortSignal) => {
      const data = await resource<{ connections: ConnectionDTO[] }>(api, "/connections", signal);
      return {
        connections: data.connections.map(({ ssl_mode, ...item }) => ({
          ...item,
          sslmode: ssl_mode,
        })),
      };
    },
    createConnection: (input: ConnectionInput) =>
      api.request("/connections", json("POST", connectionInput(input))),
    updateConnection: (id: string, input: ConnectionInput) =>
      api.request(connection(id), json("PATCH", connectionInput(input))),
    deleteConnection: (id: string) => api.request(connection(id), { method: "DELETE" }),
    testConnection: (id: string) => api.request(`${connection(id)}/test`, { method: "POST" }),
    schemas: (id: string, signal?: AbortSignal) =>
      resource<{ schemas: string[] }>(api, `${path(id)}/schemas`, signal),
    tables: (id: string, schema: string, signal?: AbortSignal) =>
      resource<{ tables: Table[] }>(api, `${path(id)}/tables?${query({ schema })}`, signal),
    details: async (id: string, schema: string, table: string, signal?: AbortSignal) =>
      (
        await resource<{ table: TableDetails }>(
          api,
          `${path(id)}/table?${query({ schema, table })}`,
          signal,
        )
      ).table,
    data: async (id: string, schema: string, table: string, page: number, signal?: AbortSignal) =>
      (
        await resource<{ page: DataPage }>(
          api,
          `${path(id)}/data?${query({ schema, table, page })}`,
          signal,
        )
      ).page,
    diff: async (
      id: string,
      reference_schema: string,
      target_schema: string,
      signal?: AbortSignal,
    ) =>
      (
        await resource<{ diff: SchemaDiff }>(
          api,
          `${path(id)}/schema-diff?${query({ reference_schema, target_schema })}`,
          signal,
        )
      ).diff,
    createDiffDraft: (
      id: string,
      input: { reference_schema: string; target_schema: string; title: string },
    ) => api.request<{ migration_id: string }>(`${path(id)}/diff-drafts`, json("POST", input)),
  };
}

export function accessLabel(level: DatabaseChoice["access_level"]) {
  return level === 2 ? "View + Edit" : "View Only";
}
