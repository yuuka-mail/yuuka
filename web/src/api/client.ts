import type { ErrorBody } from "./types.gen";

/** A non-2xx response; message is the server's error text. */
export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  // Same-origin only: the session cookie is SameSite=Strict and the
  // server never answers CORS, so there is nothing to configure here.
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: body === undefined ? { Accept: "application/json" } : { Accept: "application/json", "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  let data: unknown;
  try {
    data = await res.json();
  } catch {
    data = undefined;
  }
  if (!res.ok) {
    const msg = (data as ErrorBody | undefined)?.error ?? res.statusText;
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const getJSON = <T>(path: string) => request<T>("GET", path);
export const postJSON = <T>(path: string, body: unknown = {}) => request<T>("POST", path, body);

export function isUnauthenticated(err: unknown) {
  return err instanceof ApiError && err.status === 401;
}

