// Relative URLs reach this app's backend from any URL it is served at (live, a version
// preview or a share link); shartifacts adds the session to them.
export type Note = { id: string; text: string; author?: string; created_at: string };

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`api/${path}`, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers || {}) },
  });
  const body = await res.json();
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body as T;
}

export const listNotes = () => call<Note[]>("notes");
export const addNote = (text: string) => call<Note>("notes", { method: "POST", body: JSON.stringify({ text }) });
export const deleteNote = (id: string) => call<{ deleted: boolean }>(`notes/${id}`, { method: "DELETE" });
