export type SessionUser = {
  id: string;
  username: string;
  is_platform_admin: boolean;
};

export type AuthSession = { user: SessionUser };
export type LoginInput = { username: string; password: string };
