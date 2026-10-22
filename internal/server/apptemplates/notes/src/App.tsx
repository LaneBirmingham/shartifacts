import { useEffect, useState } from "react";
import { addNote, deleteNote, listNotes, type Note } from "./api";

declare global {
  interface Window {
    shartifacts: { version: number; live: boolean; public: boolean; readOnly: boolean; user: { name: string } | null };
  }
}

export default function App() {
  const [notes, setNotes] = useState<Note[]>([]);
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const { version, user, readOnly } = window.shartifacts;

  useEffect(() => {
    listNotes().then(setNotes, (e) => setError(String(e.message || e)));
  }, []);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!text.trim()) return;
    try {
      const note = await addNote(text);
      setNotes([note, ...notes]);
      setText("");
      setError("");
    } catch (e: any) {
      setError(e.message);
    }
  }

  async function remove(id: string) {
    try {
      await deleteNote(id);
      setNotes(notes.filter((n) => n.id !== id));
    } catch (e: any) {
      setError(e.message);
    }
  }

  return (
    <main className="notes">
      <p className="eyebrow">A FULL-STACK APP · VERSION {version}</p>
      <h1>Notes{user ? `, ${user.name}` : ""}.</h1>
      <p className="lede">React in front, Go behind, data kept on the server.</p>
      {!readOnly && (
        <form onSubmit={submit}>
          <input value={text} onChange={(e) => setText(e.target.value)} placeholder="Write something down…" aria-label="New note" />
          <button type="submit">Add note</button>
        </form>
      )}
      {error && <p className="error" role="alert">{error}</p>}
      <ul>
        {notes.map((n) => (
          <li key={n.id}>
            <p>{n.text}</p>
            <small>
              {n.author ? `${n.author} · ` : ""}
              {new Date(n.created_at).toLocaleString()}
            </small>
            {!readOnly && (
              <button className="quiet" onClick={() => remove(n.id)} aria-label="Delete note">
                ×
              </button>
            )}
          </li>
        ))}
        {notes.length === 0 && <li className="empty">Nothing yet. Your first note will appear here.</li>}
      </ul>
    </main>
  );
}
