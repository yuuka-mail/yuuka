import createFetchClient from "openapi-fetch";
import createQueryClient from "openapi-react-query";

import type { paths } from "./schema.gen";
import type { Problem } from "./types";

/**
 * A non-2xx response: the server's problem+json (RFC 9457). message is
 * its detail, for people; code is what code branches on.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code?: Problem["code"];
  /** Where in the request body it went wrong, e.g. match.all[1].value (invalid_rule). */
  readonly path?: string;
  constructor(status: number, message: string, code?: Problem["code"], path?: string) {
    super(message);
    this.status = status;
    this.code = code;
    this.path = path;
  }
}

/**
 * The typed client of openapi.json. Same-origin only: the session cookie
 * is SameSite=Strict and the server never answers CORS, so there is
 * nothing to configure. Several values of a parameter are comma separated
 * (class=human,unknown), as the contract says.
 */
export const api = createFetchClient<paths>({
  credentials: "same-origin",
  querySerializer: { array: { style: "form", explode: false } },
});

// Every non-2xx response becomes an ApiError, so callers and TanStack
// Query see one error type whichever endpoint failed.
api.use({
  async onResponse({ response }) {
    if (response.ok) return undefined;
    let p: Partial<Problem> | undefined;
    try {
      p = (await response.clone().json()) as Partial<Problem>;
    } catch {
      p = undefined;
    }
    throw new ApiError(
      response.status,
      p?.detail ?? p?.title ?? response.statusText,
      p?.code,
      p?.path,
    );
  },
});

/**
 * The body of a successful call. A failed one has already thrown ApiError
 * (the middleware above), so there is no error to look at here.
 */
export async function data<T>(call: Promise<{ data?: T }>): Promise<T> {
  return (await call).data as T;
}

/** TanStack Query hooks over api; query keys are [method, path, init]. */
export const $api = createQueryClient(api);

export function isUnauthenticated(err: unknown) {
  return err instanceof ApiError && err.status === 401;
}
