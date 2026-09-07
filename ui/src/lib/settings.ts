// Server URL + auth token, persisted in localStorage. The settings page is the one-time prompt;
// everything else reads through here.

const URL_KEY = "grove.serverUrl";
const TOKEN_KEY = "grove.token";

export function getServerUrl(): string {
  const stored = localStorage.getItem(URL_KEY);
  if (stored) return stored;
  return window.location.origin;
}

export function setServerUrl(url: string): void {
  localStorage.setItem(URL_KEY, url.replace(/\/+$/, ""));
}

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token);
}

export function hasToken(): boolean {
  return getToken().length > 0;
}
