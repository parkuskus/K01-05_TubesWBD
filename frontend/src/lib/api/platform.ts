import { json, resource, resourceID, type ApiClient } from "./resources";

export type PlatformUser = {
  id: string;
  username: string;
  is_platform_admin: boolean;
};

export type Group = { id: string; name: string; leader_id: string | null };
export type Grant = { database_id: string; level: 1 | 2 };
export type GroupDetail = { group: Group; members: PlatformUser[]; grants: Grant[] };

export function platformApi(api: ApiClient) {
  const groupPath = (id: string) => `/groups/${resourceID(id)}`;
  const userPath = (id: string) => `/users/${resourceID(id)}`;
  return {
    users: (signal?: AbortSignal) => resource<{ users: PlatformUser[] }>(api, "/users", signal),
    createUser: (username: string, password: string) =>
      api.request("/users", json("POST", { username, password })),
    updateUser: (id: string, input: { username?: string; is_platform_admin?: boolean }) =>
      api.request(userPath(id), json("PATCH", input)),
    setPassword: (id: string, password: string) =>
      api.request(`${userPath(id)}/password`, json("PUT", { password })),
    deleteUser: (id: string) => api.request(userPath(id), { method: "DELETE" }),
    groups: (signal?: AbortSignal) => resource<{ groups: Group[] }>(api, "/groups", signal),
    group: (id: string, signal?: AbortSignal) => resource<GroupDetail>(api, groupPath(id), signal),
    createGroup: (name: string) => api.request("/groups", json("POST", { name })),
    renameGroup: (id: string, name: string) => api.request(groupPath(id), json("PATCH", { name })),
    deleteGroup: (id: string) => api.request(groupPath(id), { method: "DELETE" }),
    setMember: (id: string, userID: string, member: boolean) =>
      api.request(`${groupPath(id)}/members/${resourceID(userID)}`, {
        method: member ? "PUT" : "DELETE",
      }),
    setLeader: (id: string, userID: string) =>
      api.request(`${groupPath(id)}/leader`, json("PUT", { user_id: userID })),
    setGrant: (id: string, databaseID: string, level: 1 | 2) =>
      api.request(`${groupPath(id)}/grants/${resourceID(databaseID)}`, json("PUT", { level })),
    deleteGrant: (id: string, databaseID: string) =>
      api.request(`${groupPath(id)}/grants/${resourceID(databaseID)}`, { method: "DELETE" }),
  };
}
