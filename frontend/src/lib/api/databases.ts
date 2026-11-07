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
    databases: (signal?: AbortSignal) =>
      resource<{ databases: DatabaseChoice[] }>(api, "/databases", signal),
    database: (id: string, signal?: AbortSignal) => resource<DatabaseChoice>(api, path(id), signal),
    connections: (signal?: AbortSignal) =>
      resource<{ connections: Connection[] }>(api, "/connections", signal),
    createConnection: (input: ConnectionInput) => api.request("/connections", json("POST", input)),
    updateConnection: (id: string, input: ConnectionInput) =>
      api.request(connection(id), json("PATCH", input)),
    deleteConnection: (id: string) => api.request(connection(id), { method: "DELETE" }),
    testConnection: (id: string) => api.request(`${connection(id)}/test`, { method: "POST" }),
    schemas: (id: string, signal?: AbortSignal) =>
      resource<{ schemas: string[] }>(api, `${path(id)}/schemas`, signal),
    tables: (id: string, schema: string, signal?: AbortSignal) =>
      resource<{ tables: Table[] }>(api, `${path(id)}/tables?${query({ schema })}`, signal),
    details: (id: string, schema: string, table: string, signal?: AbortSignal) =>
      resource<TableDetails>(api, `${path(id)}/table?${query({ schema, table })}`, signal),
    data: (id: string, schema: string, table: string, page: number, signal?: AbortSignal) =>
      resource<DataPage>(api, `${path(id)}/data?${query({ schema, table, page })}`, signal),
    diff: (id: string, reference_schema: string, target_schema: string, signal?: AbortSignal) =>
      resource<SchemaDiff>(
        api,
        `${path(id)}/schema-diff?${query({ reference_schema, target_schema })}`,
        signal,
      ),
    createDiffDraft: (
      id: string,
      input: { reference_schema: string; target_schema: string; title: string },
    ) => api.request<{ migration_id: string }>(`${path(id)}/diff-drafts`, json("POST", input)),
  };
}

export function accessLabel(level: DatabaseChoice["access_level"]) {
  return level === 2 ? "View + Edit" : "View Only";
}
