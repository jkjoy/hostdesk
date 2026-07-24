let csrfToken = "";

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message);
  }
}

export function setCSRF(value: string) {
  csrfToken = value;
}

export function getCSRFToken() {
  return csrfToken;
}

type ApiOptions = Omit<RequestInit, "body"> & { body?: unknown };

export async function api<T>(url: string, options: ApiOptions = {}): Promise<T> {
  const headers = new Headers(options.headers);
  let body = options.body as BodyInit | null | undefined;
  if (body && typeof body !== "string" && !(body instanceof Blob) && !(body instanceof FormData)) {
    headers.set("content-type", "application/json");
    body = JSON.stringify(body);
  }
  if (options.method && !["GET", "HEAD"].includes(options.method)) {
    headers.set("x-csrf-token", csrfToken);
  }

  const response = await fetch(url, { ...options, body, headers });
  const contentType = response.headers.get("content-type") || "";
  const data = contentType.includes("application/json") ? await response.json() : await response.text();
  if (!response.ok) {
    const message = typeof data === "object" && data && "error" in data ? String(data.error) : String(data || `请求失败 (${response.status})`);
    throw new ApiError(message, response.status);
  }
  return data as T;
}
