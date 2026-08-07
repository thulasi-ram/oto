import * as v from "valibot";

/**
 * The single HTTP entry point for the UI. Every response is parsed through a
 * valibot schema, so a backend change that breaks the contract fails loudly at
 * the boundary rather than as `undefined` three components deep.
 *
 * Paths are relative: the Vite dev server proxies them to the Go process
 * (see vite.config.ts) and in production the same origin serves both.
 */

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly problem: Problem | null,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** RFC 9457 problem+json — the only error shape oto emits. */
export const ProblemSchema = v.object({
  type: v.string(),
  title: v.string(),
  status: v.number(),
  detail: v.optional(v.string()),
  instance: v.optional(v.string()),
  request_id: v.optional(v.string()),
  errors: v.optional(
    v.array(v.object({ field: v.string(), code: v.string() })),
  ),
});
export type Problem = v.InferOutput<typeof ProblemSchema>;

export const HealthSchema = v.object({
  status: v.string(),
  service: v.string(),
  version: v.string(),
});
export type Health = v.InferOutput<typeof HealthSchema>;

async function request<TSchema extends v.GenericSchema>(
  path: string,
  schema: TSchema,
  init?: RequestInit,
): Promise<v.InferOutput<TSchema>> {
  const res = await fetch(path, {
    ...init,
    headers: { Accept: "application/json", ...init?.headers },
  });

  const body: unknown = await res.json().catch(() => null);

  if (!res.ok) {
    const problem = v.safeParse(ProblemSchema, body);
    throw new ApiError(
      res.status,
      problem.success ? problem.output : null,
      problem.success
        ? (problem.output.detail ?? problem.output.title)
        : `${res.status} ${res.statusText}`,
    );
  }

  return v.parse(schema, body);
}

/** Liveness. Proves the browser -> Vite proxy -> Go path end to end. */
export function getHealth(signal?: AbortSignal): Promise<Health> {
  return request("/healthz", HealthSchema, signal ? { signal } : {});
}
